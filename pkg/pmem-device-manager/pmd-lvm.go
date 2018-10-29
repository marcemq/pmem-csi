package pmdmanager

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/intel/pmem-csi/pkg/ndctl"
)

type pmemLvm struct {
	volumeGroups []string
}

var _ PmemDeviceManager = &pmemLvm{}
var lvsArgs = []string{"--noheadings", "-o", "lv_name,lv_path,lv_size", "--units", "B"}
var vgsArgs = []string{"--noheadings", "--nosuffix", "-o", "vg_name,vg_size,vg_free", "--units", "B"}

// NewPmemDeviceManagerLVM Instantiates a new LVM based pmem device manager
// The pre-requisite for this manager is that all the pmem regions which should be managed by
// this LMV manager are devided into namespaces and grouped as volume groups.
func NewPmemDeviceManagerLVM() (PmemDeviceManager, error) {
	ctx, err := ndctl.NewContext()
	if err != nil {
		return nil, fmt.Errorf("Failed to initialize pmem context: %s", err.Error())
	}
	volumeGroups := []string{}
	for _, bus := range ctx.GetBuses() {
		glog.Infof("NewPmemDeviceManagerLVM: Bus: %v", bus.DeviceName())
		for _, r := range bus.ActiveRegions() {
			glog.Infof("NewPmemDeviceManagerLVM: Region: %v", r.DeviceName())
			volumeGroups = append(volumeGroups, vgName(bus, r))
		}
	}
	ctx.Free()

	return &pmemLvm{
		volumeGroups: volumeGroups,
	}, nil
}

type vgInfo struct {
	name string
	size uint64
	free uint64
}

func (lvm *pmemLvm) GetCapacity() (uint64, error) {
	vgs, err := getVolumeGroups(lvm.volumeGroups)
	if err != nil {
		return 0, err
	}

	var capacity uint64
	for _, vg := range vgs {
		if vg.free > capacity {
			capacity = vg.free
		}
	}

	return capacity, nil
}

func (lvm *pmemLvm) CreateDevice(name string, size uint64) error {
	// pick a region, few possible strategies:
	// 1. pick first with enough available space: simplest, regions get filled in order;
	// 2. pick first with largest available space: regions get used round-robin, i.e. load-balanced, but does not leave large unused;
	// 3. pick first with smallest available which satisfies the request: ordered initially, but later leaves bigger free available;
	// Let's implement strategy 1 for now, simplest to code as no need to compare sizes in all regions
	// NOTE: We walk buses and regions in ndctl context, but avail.size we check in LV context
	vgs, err := getVolumeGroups(lvm.volumeGroups)
	if err != nil {
		return err
	}
	// lvcreate takes size in MBytes if no unit.
	// We use MBytes here to avoid problems with byte-granularity, as lvcreate
	// may refuse to create some arbitrary sizes.
	// Division by 1M should not result in smaller-than-asked here
	// as lvcreate will round up to next 4MB boundary.
	sizeM := int(size / (1024 * 1024))
	strSz := strconv.Itoa(sizeM)

	for _, vg := range vgs {
		if vg.free >= size {
			// lvcreate takes size in MBytes if no unit
			output, err := exec.Command("lvcreate", "-L", strSz, "-n", name, vg.name).CombinedOutput()
			glog.Infof("lvcreate output: %s\n", string(output))
			if err != nil {
				glog.Infof("lvcreate failed: %v, trying for next free region", string(output))
			} else {
				return nil
			}
		}
	}
	return fmt.Errorf("No region is having enough space required(%v)", size)
}

func (lvm *pmemLvm) DeleteDevice(name string, flush bool) error {
	device, err := lvm.GetDevice(name)
	if err != nil {
		return err
	}
	glog.Infof("DeleteDevice: Matching LVpath: %v erase:%v", device.Path, flush)
	if flush {
		flushDevice(device)
	}
	var output []byte
	output, err = exec.Command("lvremove", "-fy", device.Path).CombinedOutput()
	glog.Infof("lvremove output: %s\n", string(output))
	return err
}

func (lvm *pmemLvm) FlushDeviceData(name string) error {
	device, err := lvm.GetDevice(name)
	if err != nil {
		return err
	}
	return flushDevice(device)
}

func (lvm *pmemLvm) GetDevice(id string) (PmemDeviceInfo, error) {
	devices, err := lvm.ListDevices()
	if err != nil {
		return PmemDeviceInfo{}, err
	}
	for _, dev := range devices {
		if dev.Name == id {
			return dev, nil
		}
	}
	return PmemDeviceInfo{}, fmt.Errorf("Device not found with name %s", id)
}

func (lvm *pmemLvm) ListDevices() ([]PmemDeviceInfo, error) {
	args := append(lvsArgs, lvm.volumeGroups...)
	output, err := exec.Command("lvs", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list volumes failed : %s(lvs output: %s)", err.Error(), string(output))
	}
	return parseLVSOuput(string(output))
}

func vgName(bus *ndctl.Bus, region *ndctl.Region) string {
	return bus.DeviceName() + region.DeviceName()
}

func flushDevice(dev PmemDeviceInfo) error {
	// erase data on block device, if not disabled by driver option
	// use one iteration instead of shred's default=3 for speed
	glog.Infof("Wiping data using [shred %v]", dev.Path)
	if output, err := exec.Command("shred", "--iterations=1", dev.Path).CombinedOutput(); err != nil {
		return fmt.Errorf("device flush failure: %v(shred output:%v)", err.Error(), string(output))
	}
	return nil
}

//lvs options "lv_name,lv_path,lv_size,lv_free"
func parseLVSOuput(output string) ([]PmemDeviceInfo, error) {
	devices := []PmemDeviceInfo{}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 {
			continue
		}

		dev := PmemDeviceInfo{}
		dev.Name = fields[0]
		dev.Path = fields[1]
		dev.Size, _ = strconv.ParseUint(fields[2], 10, 64)

		devices = append(devices, dev)
	}

	return devices, nil
}

func getVolumeGroups(groups []string) ([]vgInfo, error) {
	vgs := []vgInfo{}
	args := append(vgsArgs, groups...)
	glog.Infof("Running: vgs %v", args)
	output, err := exec.Command("vgs", args...).CombinedOutput()
	glog.Infof("Output: %s", string(output))
	if err != nil {
		return vgs, fmt.Errorf("vgs failure: %s(output %s)", err.Error(), string(output))
	}
	for _, line := range strings.SplitN(string(output), "\n", len(groups)) {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 {
			return vgs, fmt.Errorf("Failed to parse vgs output line: %s", line)
		}
		vg := vgInfo{}
		vg.name = fields[0]
		vg.size, _ = strconv.ParseUint(fields[1], 10, 64)
		vg.free, _ = strconv.ParseUint(fields[2], 10, 64)
		vgs = append(vgs, vg)
	}

	return vgs, nil
}