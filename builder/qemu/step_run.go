package qemu

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/packer/helper/multistep"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
)

// stepRun runs the virtual machine
type stepRun struct {
	DiskImage bool
}

type qemuArgsTemplateData struct {
	HTTPIP      string
	HTTPPort    int
	HTTPDir     string
	OutputDir   string
	Name        string
	SSHHostPort int
}

func (s *stepRun) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	driver := state.Get("driver").(Driver)
	ui := state.Get("ui").(packer.Ui)

	// Run command is different depending whether we're booting from an
	// installation CD or a pre-baked image
	bootDrive := "once=d"
	message := "Starting VM, booting from CD-ROM"
	if s.DiskImage {
		bootDrive = "c"
		message = "Starting VM, booting disk image"
	}
	ui.Say(message)

	command, err := getCommandArgs(bootDrive, state)
	if err != nil {
		err := fmt.Errorf("Error processing QemuArgs: %s", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	if err := driver.Qemu(command...); err != nil {
		err := fmt.Errorf("Error launching VM: %s", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	return multistep.ActionContinue
}

func (s *stepRun) Cleanup(state multistep.StateBag) {
	driver := state.Get("driver").(Driver)
	ui := state.Get("ui").(packer.Ui)

	if err := driver.Stop(); err != nil {
		ui.Error(fmt.Sprintf("Error shutting down VM: %s", err))
	}
}

func getCommandArgs(bootDrive string, state multistep.StateBag) ([]string, error) {
	config := state.Get("config").(*Config)
	isoPath := state.Get("iso_path").(string)
	vncIP := config.VNCBindAddress
	vncPort := state.Get("vnc_port").(int)
	ui := state.Get("ui").(packer.Ui)
	driver := state.Get("driver").(Driver)
	vmName := config.VMName
	imgPath := filepath.Join(config.OutputDir, vmName)

	defaultArgs := make(map[string]interface{})
	var deviceArgs []string
	var driveArgs []string
	var commHostPort int

	vncPort = vncPort - config.VNCPortMin
	vnc := fmt.Sprintf("%s:%d", vncIP, vncPort)
	if config.VNCUsePassword {
		vnc = fmt.Sprintf("%s:%d,password", vncIP, vncPort)
	}

	if config.QMPEnable {
		defaultArgs["-qmp"] = fmt.Sprintf("unix:%s,server,nowait", config.QMPSocketPath)
	}

	defaultArgs["-name"] = vmName
	defaultArgs["-machine"] = fmt.Sprintf("type=%s", config.MachineType)

	if config.NetBridge == "" {
		if config.CommConfig.Comm.Type != "none" {
			commHostPort = state.Get("commHostPort").(int)
			defaultArgs["-netdev"] = fmt.Sprintf("user,id=user.0,hostfwd=tcp::%v-:%d", commHostPort, config.CommConfig.Comm.Port())
		} else {
			defaultArgs["-netdev"] = fmt.Sprintf("user,id=user.0")
		}
	} else {
		defaultArgs["-netdev"] = fmt.Sprintf("bridge,id=user.0,br=%s", config.NetBridge)
	}

	rawVersion, err := driver.Version()
	if err != nil {
		return nil, err
	}
	qemuVersion, err := version.NewVersion(rawVersion)
	if err != nil {
		return nil, err
	}
	v2 := version.Must(version.NewVersion("2.0"))

	if qemuVersion.GreaterThanOrEqual(v2) {
		if config.DiskInterface == "virtio-scsi" {
			if config.DiskImage {
				deviceArgs = append(deviceArgs, "virtio-scsi-pci,id=scsi0", "scsi-hd,bus=scsi0.0,drive=drive0")
				driveArgumentString := fmt.Sprintf("if=none,file=%s,id=drive0,cache=%s,discard=%s,format=%s", imgPath, config.DiskCache, config.DiskDiscard, config.Format)
				if config.DetectZeroes != "off" {
					driveArgumentString = fmt.Sprintf("%s,detect-zeroes=%s", driveArgumentString, config.DetectZeroes)
				}
				driveArgs = append(driveArgs, driveArgumentString)
			} else {
				deviceArgs = append(deviceArgs, "virtio-scsi-pci,id=scsi0")
				diskFullPaths := state.Get("qemu_disk_paths").([]string)
				for i, diskFullPath := range diskFullPaths {
					deviceArgs = append(deviceArgs, fmt.Sprintf("scsi-hd,bus=scsi0.0,drive=drive%d", i))
					driveArgumentString := fmt.Sprintf("if=none,file=%s,id=drive%d,cache=%s,discard=%s,format=%s", diskFullPath, i, config.DiskCache, config.DiskDiscard, config.Format)
					if config.DetectZeroes != "off" {
						driveArgumentString = fmt.Sprintf("%s,detect-zeroes=%s", driveArgumentString, config.DetectZeroes)
					}
					driveArgs = append(driveArgs, driveArgumentString)
				}
			}
		} else {
			if config.DiskImage {
				driveArgumentString := fmt.Sprintf("file=%s,if=%s,cache=%s,discard=%s,format=%s", imgPath, config.DiskInterface, config.DiskCache, config.DiskDiscard, config.Format)
				if config.DetectZeroes != "off" {
					driveArgumentString = fmt.Sprintf("%s,detect-zeroes=%s", driveArgumentString, config.DetectZeroes)
				}
				driveArgs = append(driveArgs, driveArgumentString)
			} else {
				diskFullPaths := state.Get("qemu_disk_paths").([]string)
				for _, diskFullPath := range diskFullPaths {
					driveArgumentString := fmt.Sprintf("file=%s,if=%s,cache=%s,discard=%s,format=%s", diskFullPath, config.DiskInterface, config.DiskCache, config.DiskDiscard, config.Format)
					if config.DetectZeroes != "off" {
						driveArgumentString = fmt.Sprintf("%s,detect-zeroes=%s", driveArgumentString, config.DetectZeroes)
					}
					driveArgs = append(driveArgs, driveArgumentString)
				}
			}
		}
	} else {
		driveArgs = append(driveArgs, fmt.Sprintf("file=%s,if=%s,cache=%s,format=%s", imgPath, config.DiskInterface, config.DiskCache, config.Format))
	}
	deviceArgs = append(deviceArgs, fmt.Sprintf("%s,netdev=user.0", config.NetDevice))

	if config.Headless == true {
		vncPortRaw, vncPortOk := state.GetOk("vnc_port")
		vncPass := state.Get("vnc_password")

		if vncPortOk && vncPass != nil && len(vncPass.(string)) > 0 {
			vncPort := vncPortRaw.(int)

			ui.Message(fmt.Sprintf(
				"The VM will be run headless, without a GUI. If you want to\n"+
					"view the screen of the VM, connect via VNC to vnc://%s:%d\n"+
					"with the password: %s", vncIP, vncPort, vncPass))
		} else if vncPortOk {
			vncPort := vncPortRaw.(int)

			ui.Message(fmt.Sprintf(
				"The VM will be run headless, without a GUI. If you want to\n"+
					"view the screen of the VM, connect via VNC without a password to\n"+
					"vnc://%s:%d", vncIP, vncPort))
		} else {
			ui.Message("The VM will be run headless, without a GUI, as configured.\n" +
				"If the run isn't succeeding as you expect, please enable the GUI\n" +
				"to inspect the progress of the build.")
		}
	} else {
		if qemuVersion.GreaterThanOrEqual(v2) {
			if len(config.Display) > 0 {
				if config.Display != "none" {
					defaultArgs["-display"] = config.Display
				}
			} else if !config.UseDefaultDisplay {
				defaultArgs["-display"] = "gtk"
			}
		} else {
			ui.Message("WARNING: The version of qemu  on your host doesn't support display mode.\n" +
				"The display parameter will be ignored.")
		}
	}

	cdPaths := []string{}
	// Add the installation CD to the run command
	if !config.DiskImage {
		cdPaths = append(cdPaths, isoPath)
	}
	// Add our custom CD created from cd_files, if it exists
	cdFilesPath, ok := state.Get("cd_path").(string)
	if ok {
		if cdFilesPath != "" {
			cdPaths = append(cdPaths, cdFilesPath)
		}
	}
	for i, cdPath := range cdPaths {
		if config.CDROMInterface == "" {
			driveArgs = append(driveArgs, fmt.Sprintf("file=%s,index=%d,media=cdrom", cdPath, i))
		} else if config.CDROMInterface == "virtio-scsi" {
			driveArgs = append(driveArgs, fmt.Sprintf("file=%s,if=none,index=%d,id=cdrom%d,media=cdrom", cdPath, i, i))
			deviceArgs = append(deviceArgs, "virtio-scsi-device", fmt.Sprintf("scsi-cd,drive=cdrom%d", i))
		} else {
			driveArgs = append(driveArgs, fmt.Sprintf("file=%s,if=%s,index=%d,id=cdrom%d,media=cdrom", cdPath, config.CDROMInterface, i, i))
		}
	}

	defaultArgs["-device"] = deviceArgs
	defaultArgs["-drive"] = driveArgs

	defaultArgs["-boot"] = bootDrive
	defaultArgs["-m"] = fmt.Sprintf("%dM", config.MemorySize)
	if config.CpuCount > 1 {
		defaultArgs["-smp"] = fmt.Sprintf("cpus=%d,sockets=%d", config.CpuCount, config.CpuCount)
	}
	defaultArgs["-vnc"] = vnc

	// Append the accelerator to the machine type if it is specified
	if config.Accelerator != "none" {
		defaultArgs["-machine"] = fmt.Sprintf("%s,accel=%s", defaultArgs["-machine"], config.Accelerator)
	} else {
		ui.Message("WARNING: The VM will be started with no hardware acceleration.\n" +
			"The installation may take considerably longer to finish.\n")
	}

	// Determine if we have a floppy disk to attach
	if floppyPathRaw, ok := state.GetOk("floppy_path"); ok {
		defaultArgs["-fda"] = floppyPathRaw.(string)
	} else {
		log.Println("Qemu Builder has no floppy files, not attaching a floppy.")
	}

	inArgs := make(map[string][]string)
	if len(config.QemuArgs) > 0 {
		ui.Say("Overriding default Qemu arguments with QemuArgs...")

		httpIp := state.Get("http_ip").(string)
		httpPort := state.Get("http_port").(int)
		ictx := config.ctx
		if config.CommConfig.Comm.Type != "none" {
			ictx.Data = qemuArgsTemplateData{
				httpIp,
				httpPort,
				config.HTTPDir,
				config.OutputDir,
				config.VMName,
				commHostPort,
			}
		} else {
			ictx.Data = qemuArgsTemplateData{
				HTTPIP:    httpIp,
				HTTPPort:  httpPort,
				HTTPDir:   config.HTTPDir,
				OutputDir: config.OutputDir,
				Name:      config.VMName,
			}
		}
		newQemuArgs, err := processArgs(config.QemuArgs, &ictx)
		if err != nil {
			return nil, err
		}

		// because qemu supports multiple appearances of the same
		// switch, just different values, each key in the args hash
		// will have an array of string values
		for _, qemuArgs := range newQemuArgs {
			key := qemuArgs[0]
			val := strings.Join(qemuArgs[1:], "")
			if _, ok := inArgs[key]; !ok {
				inArgs[key] = make([]string, 0)
			}
			if len(val) > 0 {
				inArgs[key] = append(inArgs[key], val)
			}
		}
	}

	// get any remaining missing default args from the default settings
	for key := range defaultArgs {
		if _, ok := inArgs[key]; !ok {
			arg := make([]string, 1)
			switch defaultArgs[key].(type) {
			case string:
				arg[0] = defaultArgs[key].(string)
			case []string:
				arg = defaultArgs[key].([]string)
			}
			inArgs[key] = arg
		}
	}

	// Check if we are missing the netDevice #6804
	if x, ok := inArgs["-device"]; ok {
		if !strings.Contains(strings.Join(x, ""), config.NetDevice) {
			inArgs["-device"] = append(inArgs["-device"], fmt.Sprintf("%s,netdev=user.0", config.NetDevice))
		}
	}

	// Flatten to array of strings
	outArgs := make([]string, 0)
	for key, values := range inArgs {
		if len(values) > 0 {
			for idx := range values {
				outArgs = append(outArgs, key, values[idx])
			}
		} else {
			outArgs = append(outArgs, key)
		}
	}

	return outArgs, nil
}

func processArgs(args [][]string, ctx *interpolate.Context) ([][]string, error) {
	var err error

	if args == nil {
		return make([][]string, 0), err
	}

	newArgs := make([][]string, len(args))
	for argsIdx, rowArgs := range args {
		parms := make([]string, len(rowArgs))
		newArgs[argsIdx] = parms
		for i, parm := range rowArgs {
			parms[i], err = interpolate.Render(parm, ctx)
			if err != nil {
				return nil, err
			}
		}
	}

	return newArgs, err
}
