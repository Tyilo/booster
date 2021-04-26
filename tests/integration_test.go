package tests

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anatol/vmtest"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

const kernelsDir = "/usr/lib/modules"

var (
	binariesDir    string
	kernelVersions map[string]string
)

func detectKernelVersion() (map[string]string, error) {
	files, err := os.ReadDir(kernelsDir)
	if err != nil {
		return nil, err
	}
	kernels := make(map[string]string)
	for _, f := range files {
		ver := f.Name()
		vmlinux := filepath.Join(kernelsDir, ver, "vmlinuz")
		if _, err := os.Stat(vmlinux); err != nil {
			continue
		}
		pkgbase, err := os.ReadFile(filepath.Join(kernelsDir, ver, "pkgbase"))
		if err != nil {
			return nil, err
		}
		pkgbase = bytes.TrimSpace(pkgbase)

		kernels[string(pkgbase)] = ver
	}
	return kernels, nil
}

func generateInitRamfs(opts Opts) (string, error) {
	file, err := os.CreateTemp("", "booster.img")
	if err != nil {
		return "", err
	}
	output := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}

	config, err := generateBoosterConfig(opts)
	if err != nil {
		return "", err
	}
	defer os.Remove(config)

	cmd := exec.Command(binariesDir+"/generator", "-force", "-initBinary", binariesDir+"/init", "-kernelVersion", opts.kernelVersion, "-output", output, "-config", config)
	if testing.Verbose() {
		log.Print("Create booster.img")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("Cannot generate booster.img: %v", err)
	}

	// check generated image integrity
	var verifyCmd *exec.Cmd
	switch opts.compression {
	case "none":
		verifyCmd = exec.Command("cpio", "-i", "--only-verify-crc", "--file", output)
	case "zstd", "":
		verifyCmd = exec.Command("zstd", "--test", output)
	case "gzip":
		verifyCmd = exec.Command("gzip", "--test", output)
	case "xz":
		verifyCmd = exec.Command("xz", "--test", output)
	case "lz4":
		verifyCmd = exec.Command("lz4", "--test", output)
	default:
		return "", fmt.Errorf("Unknown compression: %s", opts.compression)
	}
	if testing.Verbose() {
		verifyCmd.Stdout = os.Stdout
		verifyCmd.Stderr = os.Stderr
	}
	if err := verifyCmd.Run(); err != nil {
		return "", fmt.Errorf("unable to verify integrity of the output image %s: %v", output, err)
	}

	return output, nil
}

type NetworkConfig struct {
	Interfaces string `yaml:",omitempty"` // comma-separaed list of interfaces to initialize at early-userspace

	Dhcp bool `yaml:",omitempty"`

	Ip         string `yaml:",omitempty"` // e.g. 10.0.2.15/24
	Gateway    string `yaml:",omitempty"` // e.g. 10.0.2.255
	DNSServers string `yaml:"dns_servers,omitempty"`
}
type GeneratorConfig struct {
	Network              *NetworkConfig `yaml:",omitempty"`
	Universal            bool           `yaml:",omitempty"`
	Modules              string         `yaml:",omitempty"`
	ModulesForceLoad     string         `yaml:"modules_force_load,omitempty"` // comma separated list of extra modules to load at the boot time
	Compression          string         `yaml:",omitempty"`
	MountTimeout         string         `yaml:"mount_timeout,omitempty"`
	ExtraFiles           string         `yaml:"extra_files,omitempty"`
	StripBinaries        bool           `yaml:"strip,omitempty"` // strip symbols from the binaries, shared libraries and kernel modules
	EnableVirtualConsole bool           `yaml:"vconsole,omitempty"`
	EnableLVM            bool           `yaml:"enable_lvm"`
}

func generateBoosterConfig(opts Opts) (string, error) {
	file, err := os.CreateTemp("", "booster.yaml")
	if err != nil {
		return "", err
	}

	var conf GeneratorConfig

	if opts.enableTangd { // tang requires network enabled
		net := &NetworkConfig{}
		conf.Network = net

		if opts.useDhcp {
			net.Dhcp = true
		} else {
			net.Ip = "10.0.2.15/24"
		}

		net.Interfaces = opts.activeNetIfaces
	}
	conf.Universal = true
	conf.Compression = opts.compression
	conf.MountTimeout = strconv.Itoa(opts.mountTimeout) + "s"
	conf.ExtraFiles = opts.extraFiles
	conf.StripBinaries = opts.stripBinaries
	conf.EnableVirtualConsole = opts.enableVirtualConsole
	conf.EnableLVM = opts.enableLVM
	conf.ModulesForceLoad = opts.modulesForceLoad

	data, err := yaml.Marshal(&conf)
	if err != nil {
		return "", err
	}
	if _, err = file.Write(data); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return file.Name(), nil
}

type Opts struct {
	params               []string
	compression          string
	prompt               string
	password             string
	modulesForceLoad     string
	enableTangd          bool
	useDhcp              bool
	activeNetIfaces      string
	enableTpm2           bool
	kernelVersion        string // kernel version
	kernelArgs           []string
	disk                 string
	disks                []vmtest.QemuDisk
	mountTimeout         int // in seconds
	extraFiles           string
	checkVmState         func(vm *vmtest.Qemu, t *testing.T)
	forceKill            bool // if true then kill VM rather than do a graceful shutdown
	stripBinaries        bool
	enableVirtualConsole bool
	enableLVM            bool
}

func boosterTest(opts Opts) func(*testing.T) {
	if opts.checkVmState == nil {
		// default simple check
		opts.checkVmState = func(vm *vmtest.Qemu, t *testing.T) {
			if err := vm.ConsoleExpect("Hello, booster!"); err != nil {
				t.Fatal(err)
			}
		}
	}
	const defaultLuksPassword = "1234"
	if opts.prompt != "" && opts.password == "" {
		opts.password = defaultLuksPassword
	}

	return func(t *testing.T) {
		// TODO: make this test run in parallel

		if kernel, ok := kernelVersions["linux"]; ok {
			opts.kernelVersion = kernel
		} else {
			t.Fatal("System does not have 'linux' package installed needed for the integration tests")
		}

		initRamfs, err := generateInitRamfs(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(initRamfs)

		params := []string{"-m", "8G", "-smp", strconv.Itoa(runtime.NumCPU())}
		if os.Getenv("TEST_DISABLE_KVM") != "1" {
			params = append(params, "-enable-kvm", "-cpu", "host")
		}

		kernelArgs := append(opts.kernelArgs, "booster.debug")

		if opts.disk != "" && len(opts.disks) != 0 {
			t.Fatal("Opts.disk and Opts.disks cannot be specified together")
		}
		var disks []vmtest.QemuDisk
		if opts.disk != "" {
			disks = []vmtest.QemuDisk{{opts.disk, "raw"}}
		} else {
			disks = opts.disks
		}
		for _, d := range disks {
			if err := checkAsset(d.Path); err != nil {
				t.Fatal(err)
			}
		}

		if opts.enableTangd {
			tangd, err := NewTangServer("assets/tang")
			if err != nil {
				t.Fatal(err)
			}
			defer tangd.Stop()
			// using command directly like one below does not work as extra info is printed to stderr and QEMU incorrectly
			// assumes it is a part of HTTP reply
			// guestfwd=tcp:10.0.2.100:5697-cmd:/usr/lib/tangd ./assets/tang 2>/dev/null

			params = append(params, "-nic", fmt.Sprintf("user,id=n1,restrict=on,guestfwd=tcp:10.0.2.100:5697-tcp:localhost:%d", tangd.port))
		}

		if opts.enableTpm2 {
			cmd := exec.Command("swtpm", "socket", "--tpmstate", "dir=assets/tpm2", "--tpm2", "--ctrl", "type=unixio,path=assets/swtpm-sock", "--flags", "not-need-init")
			if testing.Verbose() {
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			defer os.Remove("assets/swtpm-sock") // sometimes process crash leaves this file

			// wait till swtpm really starts
			if err := waitForFile("assets/swtpm-sock", 5*time.Second); err != nil {
				t.Fatal(err)
			}

			params = append(params, "-chardev", "socket,id=chrtpm,path=assets/swtpm-sock", "-tpmdev", "emulator,id=tpm0,chardev=chrtpm", "-device", "tpm-tis,tpmdev=tpm0")
		}

		// to enable network dump
		// params = append(params, "-object", "filter-dump,id=f1,netdev=n1,file=network.dat")

		params = append(params, opts.params...)

		options := vmtest.QemuOptions{
			OperatingSystem: vmtest.OS_LINUX,
			Kernel:          filepath.Join(kernelsDir, opts.kernelVersion, "vmlinuz"),
			InitRamFs:       initRamfs,
			Params:          params,
			Append:          kernelArgs,
			Disks:           disks,
			Verbose:         testing.Verbose(),
			Timeout:         40 * time.Second,
		}
		vm, err := vmtest.NewQemu(&options)
		if err != nil {
			t.Fatal(err)
		}
		if opts.forceKill {
			defer vm.Kill()
		} else {
			defer vm.Shutdown()
		}

		if opts.prompt != "" {
			if err := vm.ConsoleExpect(opts.prompt); err != nil {
				t.Fatal(err)
			}
			if err := vm.ConsoleWrite(opts.password + "\n"); err != nil {
				t.Fatal(err)
			}
		}
		opts.checkVmState(vm, t)
	}
}

func waitForFile(filename string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		_, err := os.Stat(filename)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("waitForFile: %v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %v", filename)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func compileBinaries(dir string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Build init binary
	if err := os.Chdir("../init"); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", dir+"/init")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if testing.Verbose() {
		log.Print("Call 'go build' for init")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Cannot build init binary: %v", err)
	}

	// Generate initramfs
	if err := os.Chdir("../generator"); err != nil {
		return err
	}
	cmd = exec.Command("go", "build", "-o", dir+"/generator")
	if testing.Verbose() {
		log.Print("Call 'go build' for generator")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Cannot build generator binary: %v", err)
	}

	return os.Chdir(cwd)
}

func runSshCommand(t *testing.T, conn *ssh.Client, command string) string {
	sessAnalyze, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sessAnalyze.Close()

	out, err := sessAnalyze.CombinedOutput(command)
	if err != nil {
		t.Fatal(err)
	}

	return string(out)
}

type assetGenerator struct {
	script string
	env    []string
}

var assetGenerators = make(map[string]assetGenerator)

func initAssetsGenerators() error {
	_ = os.Mkdir("assets", 0755)

	if exists := fileExists("assets/init"); !exists {
		if err := exec.Command("gcc", "-static", "-o", "assets/init", "init/init.c").Run(); err != nil {
			return err
		}
	}

	if exists := fileExists("assets/tang/adv.jwk"); !exists {
		if err := shell("generate_asset_tang.sh"); err != nil {
			return err
		}
	}

	if exists := fileExists("assets/tpm2/tpm2-00.permall"); !exists {
		if err := shell("generate_asset_swtpm.sh"); err != nil {
			return err
		}
	}

	assetGenerators["assets/ext4.img"] = assetGenerator{"generate_asset_ext4.sh", []string{"OUTPUT=assets/ext4.img", "FS_UUID=5c92fc66-7315-408b-b652-176dc554d370", "FS_LABEL=atestlabel12"}}
	assetGenerators["assets/luks1.img"] = assetGenerator{"generate_asset_luks.sh", []string{"OUTPUT=assets/luks1.img", "LUKS_VERSION=1", "LUKS_PASSWORD=1234", "LUKS_UUID=f0c89fd5-7e1e-4ecc-b310-8cd650bd5415", "FS_UUID=ec09a1ea-d43c-4262-b701-bf2577a9ab27"}}
	assetGenerators["assets/luks2.img"] = assetGenerator{"generate_asset_luks.sh", []string{"OUTPUT=assets/luks2.img", "LUKS_VERSION=2", "LUKS_PASSWORD=1234", "LUKS_UUID=639b8fdd-36ba-443e-be3e-e5b335935502", "FS_UUID=7bbf9363-eb42-4476-8c1c-9f1f4d091385"}}
	assetGenerators["assets/luks1.clevis.tpm2.img"] = assetGenerator{"generate_asset_luks.sh", []string{"OUTPUT=assets/luks1.clevis.tpm2.img", "LUKS_VERSION=1", "LUKS_PASSWORD=1234", "LUKS_UUID=28c2e412-ab72-4416-b224-8abd116d6f2f", "FS_UUID=2996cec0-16fd-4f1d-8bf3-6606afa77043", "CLEVIS_PIN=tpm2", "CLEVIS_CONFIG={}"}}
	assetGenerators["assets/luks1.clevis.tang.img"] = assetGenerator{"generate_asset_luks.sh", []string{"OUTPUT=assets/luks1.clevis.tang.img", "LUKS_VERSION=1", "LUKS_PASSWORD=1234", "LUKS_UUID=4cdaa447-ef43-42a6-bfef-89ebb0c61b05", "FS_UUID=c23aacf4-9e7e-4206-ba6c-af017934e6fa", "CLEVIS_PIN=tang", `CLEVIS_CONFIG={"url":"http://10.0.2.100:5697", "adv":"assets/tang/adv.jwk"}`}}
	assetGenerators["assets/luks2.clevis.tpm2.img"] = assetGenerator{"generate_asset_luks.sh", []string{"OUTPUT=assets/luks2.clevis.tpm2.img", "LUKS_VERSION=2", "LUKS_PASSWORD=1234", "LUKS_UUID=3756ba2c-1505-4283-8f0b-b1d1bd7b844f", "FS_UUID=c3cc0321-fba8-42c3-ad73-d13f8826d8d7", "CLEVIS_PIN=tpm2", "CLEVIS_CONFIG={}"}}
	assetGenerators["assets/luks2.clevis.tang.img"] = assetGenerator{"generate_asset_luks.sh", []string{"OUTPUT=assets/luks2.clevis.tang.img", "LUKS_VERSION=2", "LUKS_PASSWORD=1234", "LUKS_UUID=f2473f71-9a68-4b16-ae54-8f942b2daf50", "FS_UUID=7acb3a9e-9b50-4aa2-9965-e41ae8467d8a", "CLEVIS_PIN=tang", `CLEVIS_CONFIG={"url":"http://10.0.2.100:5697", "adv":"assets/tang/adv.jwk"}`}}
	assetGenerators["assets/lvm.img"] = assetGenerator{"generate_asset_lvm.sh", []string{"OUTPUT=assets/lvm.img", "FS_UUID=74c9e30c-506f-4106-9f61-a608466ef29c", "FS_LABEL=lvmr00t"}}
	assetGenerators["assets/archlinux.ext4.raw"] = assetGenerator{"generate_asset_archlinux_ext4.sh", []string{"OUTPUT=assets/archlinux.ext4.raw"}}
	assetGenerators["assets/archlinux.btrfs.raw"] = assetGenerator{"generate_asset_archlinux_btrfs.sh", []string{"OUTPUT=assets/archlinux.btrfs.raw", "LUKS_PASSWORD=hello"}}

	return nil
}

func checkAsset(file string) error {
	if !strings.HasPrefix(file, "assets/") {
		return nil
	}

	gen, ok := assetGenerators[file]
	if !ok {
		return fmt.Errorf("no generator for asset %s", file)
	}
	if exists := fileExists(file); exists {
		return nil
	}

	if testing.Verbose() {
		fmt.Printf("Generating asset %s\n", file)
	}
	return shell(gen.script, gen.env...)
}

func shell(script string, env ...string) error {
	sh := exec.Command("bash", "-o", "errexit", script)
	sh.Env = append(os.Environ(), env...)

	if testing.Verbose() {
		sh.Stdout = os.Stdout
		sh.Stderr = os.Stderr
	}
	return sh.Run()
}

func fileExists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}

func TestBooster(t *testing.T) {
	var err error
	kernelVersions, err = detectKernelVersion()
	if err != nil {
		t.Fatalf("unable to detect current Linux version: %v", err)
	}

	binariesDir = t.TempDir()
	if err := compileBinaries(binariesDir); err != nil {
		t.Fatal(err)
	}

	if err := initAssetsGenerators(); err != nil {
		t.Fatal(err)
	}

	// TODO: add a test to verify the emergency shell functionality
	// VmTest uses sockets for console and it seems does not like the shell we launch

	// note that assets are generated using ./assets_generator tool
	t.Run("Ext4.UUID", boosterTest(Opts{
		compression: "zstd",
		disk:        "assets/ext4.img",
		kernelArgs:  []string{"root=UUID=5c92fc66-7315-408b-b652-176dc554d370", "rootflags=user_xattr,nobarrier"},
	}))
	t.Run("Ext4.MountFlags", boosterTest(Opts{
		compression: "none",
		disk:        "assets/ext4.img",
		kernelArgs:  []string{"root=UUID=5c92fc66-7315-408b-b652-176dc554d370", "rootflags=user_xattr,noatime,nobarrier,nodev,dirsync,lazytime,nolazytime,dev,rw,ro", "rw"},
	}))
	t.Run("Ext4.Label", boosterTest(Opts{
		compression: "gzip",
		disk:        "assets/ext4.img",
		kernelArgs:  []string{"root=LABEL=atestlabel12"},
	}))

	t.Run("DisableConcurrentModuleLoading", boosterTest(Opts{
		disk:       "assets/luks2.img",
		prompt:     "Enter passphrase for luks-639b8fdd-36ba-443e-be3e-e5b335935502:",
		kernelArgs: []string{"rd.luks.uuid=639b8fdd-36ba-443e-be3e-e5b335935502", "root=UUID=7bbf9363-eb42-4476-8c1c-9f1f4d091385", "booster.disable_concurrent_module_loading"},
	}))

	// verifies module force loading + modprobe command-line parameters
	t.Run("Vfio", boosterTest(Opts{
		modulesForceLoad: "vfio_pci,vfio,vfio_iommu_type1,vfio_virqfd",
		params:           []string{"-net", "user,hostfwd=tcp::10022-:22", "-net", "nic"},
		disks:            []vmtest.QemuDisk{{"assets/archlinux.ext4.raw", "raw"}},
		kernelArgs:       []string{"root=/dev/sda", "rw", "vfio-pci.ids=1002:67df,1002:aaf0"},

		checkVmState: func(vm *vmtest.Qemu, t *testing.T) {
			config := &ssh.ClientConfig{
				User:            "root",
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			}

			conn, err := ssh.Dial("tcp", ":10022", config)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			dmesg := runSshCommand(t, conn, "dmesg")
			if !strings.Contains(dmesg, "loading module vfio_pci params=\"ids=1002:67df,1002:aaf0\"") {
				t.Fatal("expecting vfio_pci module loading")
			}
			if !strings.Contains(dmesg, "vfio_pci: add [1002:67df[ffffffff:ffffffff]] class 0x000000/00000000") {
				t.Fatal("expecting vfio_pci 1002:67df device")
			}
			if !strings.Contains(dmesg, "vfio_pci: add [1002:aaf0[ffffffff:ffffffff]] class 0x000000/00000000") {
				t.Fatal("expecting vfio_pci 1002:aaf0 device")
			}
			re := regexp.MustCompile(`booster: udev event {Header:add@/bus/pci/drivers/vfio-pci Action:add Devpath:/bus/pci/drivers/vfio-pci Subsystem:drivers Seqnum:\d+ Vars:map\[ACTION:add DEVPATH:/bus/pci/drivers/vfio-pci SEQNUM:\d+ SUBSYSTEM:drivers]}`)
			if !re.MatchString(dmesg) {
				t.Fatal("expecting vfio_pci module loading udev event")
			}
		},
	}))

	t.Run("NonFormattedDrive", boosterTest(Opts{
		compression: "none",
		disks: []vmtest.QemuDisk{
			{ /* represents non-formatted drive */ "integration_test.go", "raw"},
			{"assets/ext4.img", "raw"},
		},
		kernelArgs: []string{"root=UUID=5c92fc66-7315-408b-b652-176dc554d370"},
	}))

	t.Run("XZImageCompression", boosterTest(Opts{
		compression: "xz",
		disk:        "assets/ext4.img",
		kernelArgs:  []string{"root=UUID=5c92fc66-7315-408b-b652-176dc554d370"},
	}))
	t.Run("GzipImageCompression", boosterTest(Opts{
		compression: "gzip",
		disk:        "assets/ext4.img",
		kernelArgs:  []string{"root=UUID=5c92fc66-7315-408b-b652-176dc554d370"},
	}))
	t.Run("Lz4ImageCompression", boosterTest(Opts{
		compression: "lz4",
		disk:        "assets/ext4.img",
		kernelArgs:  []string{"root=UUID=5c92fc66-7315-408b-b652-176dc554d370"},
	}))

	t.Run("MountTimeout", boosterTest(Opts{
		compression:  "xz",
		mountTimeout: 1,
		forceKill:    true,
		checkVmState: func(vm *vmtest.Qemu, t *testing.T) {
			if err := vm.ConsoleExpect("Timeout waiting for root filesystem"); err != nil {
				t.Fatal(err)
			}
		},
	}))
	t.Run("Fsck", boosterTest(Opts{
		compression: "none",
		disk:        "assets/ext4.img",
		kernelArgs:  []string{"root=LABEL=atestlabel12"},
		extraFiles:  "fsck,fsck.ext4",
	}))
	t.Run("VirtualConsole", boosterTest(Opts{
		compression:          "none",
		disk:                 "assets/ext4.img",
		kernelArgs:           []string{"root=LABEL=atestlabel12"},
		enableVirtualConsole: true,
	}))
	t.Run("StripBinaries", boosterTest(Opts{
		disk:          "assets/luks2.clevis.tpm2.img",
		enableTpm2:    true,
		stripBinaries: true,
		kernelArgs:    []string{"rd.luks.uuid=3756ba2c-1505-4283-8f0b-b1d1bd7b844f", "root=UUID=c3cc0321-fba8-42c3-ad73-d13f8826d8d7"},
	}))

	t.Run("LUKS1.WithName", boosterTest(Opts{
		disk:       "assets/luks1.img",
		prompt:     "Enter passphrase for cryptroot:",
		kernelArgs: []string{"rd.luks.name=f0c89fd5-7e1e-4ecc-b310-8cd650bd5415=cryptroot", "root=/dev/mapper/cryptroot", "rd.luks.options=discard"},
	}))
	t.Run("LUKS1.WithUUID", boosterTest(Opts{
		disk:       "assets/luks1.img",
		prompt:     "Enter passphrase for luks-f0c89fd5-7e1e-4ecc-b310-8cd650bd5415:",
		kernelArgs: []string{"rd.luks.uuid=f0c89fd5-7e1e-4ecc-b310-8cd650bd5415", "root=UUID=ec09a1ea-d43c-4262-b701-bf2577a9ab27"},
	}))

	t.Run("LUKS2.WithName", boosterTest(Opts{
		disk:       "assets/luks2.img",
		prompt:     "Enter passphrase for cryptroot:",
		kernelArgs: []string{"rd.luks.name=639b8fdd-36ba-443e-be3e-e5b335935502=cryptroot", "root=/dev/mapper/cryptroot"},
	}))
	t.Run("LUKS2.WithUUID", boosterTest(Opts{
		disk:       "assets/luks2.img",
		prompt:     "Enter passphrase for luks-639b8fdd-36ba-443e-be3e-e5b335935502:",
		kernelArgs: []string{"rd.luks.uuid=639b8fdd-36ba-443e-be3e-e5b335935502", "root=UUID=7bbf9363-eb42-4476-8c1c-9f1f4d091385"},
	}))
	t.Run("LUKS2.WithQuotesOverUUID", boosterTest(Opts{
		disk:       "assets/luks2.img",
		prompt:     "Enter passphrase for luks-639b8fdd-36ba-443e-be3e-e5b335935502:",
		kernelArgs: []string{"rd.luks.uuid=\"639b8fdd-36ba-443e-be3e-e5b335935502\"", "root=UUID=\"7bbf9363-eb42-4476-8c1c-9f1f4d091385\""},
	}))

	t.Run("LUKS1.Clevis.Tang", boosterTest(Opts{
		disk:        "assets/luks1.clevis.tang.img",
		enableTangd: true,
		kernelArgs:  []string{"rd.luks.uuid=4cdaa447-ef43-42a6-bfef-89ebb0c61b05", "root=UUID=c23aacf4-9e7e-4206-ba6c-af017934e6fa"},
	}))
	t.Run("LUKS2.Clevis.Tang", boosterTest(Opts{
		disk:        "assets/luks2.clevis.tang.img",
		enableTangd: true,
		kernelArgs:  []string{"rd.luks.uuid=f2473f71-9a68-4b16-ae54-8f942b2daf50", "root=UUID=7acb3a9e-9b50-4aa2-9965-e41ae8467d8a"},
	}))
	t.Run("LUKS2.Clevis.Tang.DHCP", boosterTest(Opts{
		disk:            "assets/luks2.clevis.tang.img",
		enableTangd:     true,
		useDhcp:         true,
		activeNetIfaces: "52-54-00-12-34-53,52:54:00:12:34:56,52:54:00:12:34:57", // 52:54:00:12:34:56 is QEMU's NIC address
		kernelArgs:      []string{"rd.luks.uuid=f2473f71-9a68-4b16-ae54-8f942b2daf50", "root=UUID=7acb3a9e-9b50-4aa2-9965-e41ae8467d8a"},
	}))
	t.Run("InactiveNetwork", boosterTest(Opts{
		disk:            "assets/luks2.clevis.tang.img",
		enableTangd:     true,
		useDhcp:         true,
		activeNetIfaces: "52:54:00:12:34:57", // 52:54:00:12:34:56 is QEMU's NIC address
		kernelArgs:      []string{"rd.luks.uuid=f2473f71-9a68-4b16-ae54-8f942b2daf50", "root=UUID=7acb3a9e-9b50-4aa2-9965-e41ae8467d8a"},

		mountTimeout: 10,
		forceKill:    true,
		checkVmState: func(vm *vmtest.Qemu, t *testing.T) {
			if err := vm.ConsoleExpect("Timeout waiting for root filesystem"); err != nil {
				t.Fatal(err)
			}
		},
	}))

	t.Run("LUKS1.Clevis.Tpm2", boosterTest(Opts{
		disk:       "assets/luks1.clevis.tpm2.img",
		enableTpm2: true,
		kernelArgs: []string{"rd.luks.uuid=28c2e412-ab72-4416-b224-8abd116d6f2f", "root=UUID=2996cec0-16fd-4f1d-8bf3-6606afa77043"},
	}))
	t.Run("LUKS2.Clevis.Tpm2", boosterTest(Opts{
		disk:       "assets/luks2.clevis.tpm2.img",
		enableTpm2: true,
		kernelArgs: []string{"rd.luks.uuid=3756ba2c-1505-4283-8f0b-b1d1bd7b844f", "root=UUID=c3cc0321-fba8-42c3-ad73-d13f8826d8d7"},
	}))

	t.Run("LVM.Path", boosterTest(Opts{
		enableLVM:  true,
		disk:       "assets/lvm.img",
		kernelArgs: []string{"root=/dev/booster_test_vg/booster_test_lv"},
	}))
	t.Run("LVM.UUID", boosterTest(Opts{
		enableLVM:  true,
		disk:       "assets/lvm.img",
		kernelArgs: []string{"root=UUID=74c9e30c-506f-4106-9f61-a608466ef29c"},
	}))

	// boot Arch userspace (with systemd) against all installed linux packages
	for pkg, ver := range kernelVersions {
		compression := "zstd"
		if pkg == "linux-lts" {
			compression = "gzip"
		}
		checkVmState := func(vm *vmtest.Qemu, t *testing.T) {
			config := &ssh.ClientConfig{
				User:            "root",
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			}

			conn, err := ssh.Dial("tcp", ":10022", config)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			sess, err := conn.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()

			out, err := sess.CombinedOutput("systemd-analyze")
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(string(out), "(initrd)") {
				t.Fatalf("expect initrd time stats in systemd-analyze, got '%s'", string(out))
			}

			// check writing to kmesg works
			sess3, err := conn.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer sess3.Close()
			out, err = sess3.CombinedOutput("dmesg | grep -i booster")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), "Switching to the new userspace now") {
				t.Fatalf("expected to see debug output from booster")
			}

			sessShutdown, err := conn.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer sessShutdown.Close()
			// Arch Linux 5.4 does not shutdown with QEMU's 'shutdown' event for some reason. Force shutdown from ssh session.
			_, _ = sessShutdown.CombinedOutput("shutdown now")
		}

		// simple ext4 image
		t.Run("ArchLinux.ext4."+pkg, boosterTest(Opts{
			kernelVersion: ver,
			compression:   compression,
			params:        []string{"-net", "user,hostfwd=tcp::10022-:22", "-net", "nic"},
			disks:         []vmtest.QemuDisk{{"assets/archlinux.ext4.raw", "raw"}},
			// If you need more debug logs append kernel args: "systemd.log_level=debug", "udev.log-priority=debug", "systemd.log_target=console", "log_buf_len=8M"
			kernelArgs:   []string{"root=/dev/sda", "rw"},
			checkVmState: checkVmState,
		}))

		// more complex setup with LUKS and btrfs subvolumes
		t.Run("ArchLinux.btrfs."+pkg, boosterTest(Opts{
			kernelVersion: ver,
			compression:   compression,
			params:        []string{"-net", "user,hostfwd=tcp::10022-:22", "-net", "nic"},
			disks:         []vmtest.QemuDisk{{"assets/archlinux.btrfs.raw", "raw"}},
			kernelArgs:    []string{"rd.luks.uuid=724151bb-84be-493c-8e32-53e123c8351b", "root=UUID=15700169-8c12-409d-8781-37afa98442a8", "rootflags=subvol=@", "rw", "quiet", "nmi_watchdog=0", "kernel.unprivileged_userns_clone=0", "net.core.bpf_jit_harden=2", "apparmor=1", "lsm=lockdown,yama,apparmor", "systemd.unified_cgroup_hierarchy=1", "add_efi_memmap"},
			prompt:        "Enter passphrase for luks-724151bb-84be-493c-8e32-53e123c8351b:",
			password:      "hello",
			checkVmState:  checkVmState,
		}))
	}
}
