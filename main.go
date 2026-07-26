package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ——— bridge ———

const bridgeName = "mdbr0"
const bridgeIP = "10.100.0.1"
const bridgeSubnet = "10.100.0.0/24"

func ensureBridge() {
	if _, err := os.Stat("/sys/class/net/" + bridgeName); err == nil {
		return
	}
	runCmd("ip", "link", "add", bridgeName, "type", "bridge")
	runCmd("ip", "addr", "add", bridgeIP+"/24", "dev", bridgeName)
	runCmd("ip", "link", "set", bridgeName, "up")
	fmt.Printf("[bridge] %s created at %s\n", bridgeName, bridgeIP)
}

func allocateIP() string {
	data, _ := os.ReadFile("/tmp/mdbr0-ip-last")
	last := 1
	if len(data) > 0 {
		last, _ = strconv.Atoi(string(data))
	}
	next := last + 1
	os.WriteFile("/tmp/mdbr0-ip-last", []byte(strconv.Itoa(next)), 0644)
	return fmt.Sprintf("10.100.0.%d", next)
}

// 这个 demo 没有加 CLONE_NEWUSER(user namespace)。
// 加了之后容器里的"root"会映射成宿主机上一个无权限的普通用户,
// mount/mknod 这类系统调用会被内核直接拒绝——从根上堵死设备逃逸这条路。
// 现在故意不加,是因为这个工具还在用来练习/演示逃逸技术;
// 如果以后想让它变成一个"正经"的隔离环境,这是必须补的一项。

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s run [cmd] [args...]\n", os.Args[0])
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		run()
	case "child":
		child()
	default:
		panic("Unknown command")
	}
}

func run() {
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: %s run <image>[:tag] [command...]\n", os.Args[0])
		os.Exit(1)
	}

	// 解析 "alpine" 或 "python:3.11"
	image := args[0]
	tag := "latest"
	if idx := strings.Index(image, ":"); idx != -1 {
		tag = image[idx+1:]
		image = image[:idx]
	}
	cmdArgs := args[1:]

	// --no-entry: 跳过镜像的 entrypoint，直接跑用户命令
	skipEntrypoint := false
	if len(cmdArgs) > 0 && cmdArgs[0] == "--no-entry" {
		skipEntrypoint = true
		cmdArgs = cmdArgs[1:]
	}

	result, err := pullImage(image, tag)
	must(err)

	// 用户没指定命令，用镜像默认的
	if len(cmdArgs) == 0 {
		cmdArgs = result.Cmd
		if len(cmdArgs) == 0 {
			cmdArgs = []string{"/bin/sh"}
		}
	}
	// 镜像有 entrypoint 且不跳过，就排在命令前面
	if !skipEntrypoint {
		cmdArgs = append(result.Entrypoint, cmdArgs...)
	}

	hostPID := os.Getpid()
	fmt.Printf("[run]   %s:%s → %v as PID %d\n", image, tag, cmdArgs, hostPID)

	overlayBase := fmt.Sprintf("/tmp/maingo-%d", hostPID)
	upperDir := overlayBase + "/upper"
	workDir := overlayBase + "/work"
	mergedDir := overlayBase + "/merged"
	vethHost := fmt.Sprintf("veth0-%d", hostPID)
	vethCtr := fmt.Sprintf("veth1-%d", hostPID)
	cgroupName := fmt.Sprintf("maingo-%d", hostPID)

	ctrIP := allocateIP()

	must(os.MkdirAll(upperDir, 0755))
	must(os.MkdirAll(workDir, 0755))
	must(os.MkdirAll(mergedDir, 0755))

	defer safeRemoveAll(overlayBase)
	defer cleanupCgroup(cgroupName)
	defer cleanupVeth(vethHost)

	r, w, err := os.Pipe()
	must(err)

	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, cmdArgs...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{r}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWNET,
	}
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: 0, Size: 65536},
	}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: 0, Size: 65536},
	}
	// 先加镜像的环境变量，再拼我们自己的——镜像的 Env 可能覆盖宿主机的
	cmd.Env = append(os.Environ(), result.Env...)
	cmd.Env = append(cmd.Env,
		"MAINGO_MERGED="+mergedDir,
		"MAINGO_UPPER="+upperDir,
		"MAINGO_WORK="+workDir,
		"MAINGO_LOWER="+result.LowerDirs,
		"MAINGO_CGROUP="+cgroupName)

	must(cmd.Start())
	r.Close()

	ensureBridge()
	setupVeth(cmd.Process.Pid, vethHost, vethCtr, ctrIP)
	setupNAT()

	w.Write([]byte{1})
	w.Close()

	must(cmd.Wait())
}

func child() {
	fmt.Printf("[child] Running %v as PID %d\n", os.Args[2:], os.Getpid())

	must(syscall.Sethostname([]byte("container")))

	// 必须在任何 mount 操作之前执行。
	// CLONE_NEWNS 只是给了子进程一份 mount 表的独立拷贝，
	// 但拷贝出来的挂载点默认继承宿主机的 propagation 类型（多数发行版根挂载点是 shared）。
	// shared 状态下，容器里新增的任何挂载（包括后面的 overlay、以及测试用的宿主机磁盘挂载）
	// 都会自动同步回宿主机自己的 mount namespace。
	// 把根挂载点递归改成 MS_PRIVATE，彻底切断这个双向同步，
	// 容器里再怎么 mount，宿主机那边都看不见、也不会被波及。
	must(syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""))

	setupCgroup(os.Getenv("MAINGO_CGROUP"))
	waitForNetwork()

	mergedDir := os.Getenv("MAINGO_MERGED")
	upperDir := os.Getenv("MAINGO_UPPER")
	workDir := os.Getenv("MAINGO_WORK")

	lowerDir := os.Getenv("MAINGO_LOWER")
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerDir, upperDir, workDir)
	must(syscall.Mount("overlay", mergedDir, "overlay", 0, opts))
	fmt.Printf("[overlay] mounted\n")

	must(syscall.Chroot(mergedDir))
	must(syscall.Chdir("/"))
	must(os.MkdirAll("/proc", 0555))
	must(syscall.Mount("proc", "proc", "proc", 0, ""))

	cmd := exec.Command(os.Args[2], os.Args[3:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	must(cmd.Run())

	must(syscall.Unmount("proc", 0))
}

func setupVeth(childPID int, vethHost, vethCtr, ctrIP string) {
	pid := strconv.Itoa(childPID)

	runCmdSilent("ip", "link", "del", vethHost)

	runCmd("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethCtr)
	runCmd("ip", "link", "set", vethCtr, "netns", pid)

	// 宿主端插进网桥，不配 IP——bridge 自己有一个 IP 当网关
	runCmd("ip", "link", "set", vethHost, "master", bridgeName)
	runCmd("ip", "link", "set", vethHost, "up")

	// 容器端：配 IP、设默认网关、启 lo
	runCmd("nsenter", "-t", pid, "-n", "ip", "addr", "add", ctrIP+"/24", "dev", vethCtr)
	runCmd("nsenter", "-t", pid, "-n", "ip", "link", "set", vethCtr, "up")
	runCmd("nsenter", "-t", pid, "-n", "ip", "link", "set", "lo", "up")
	runCmd("nsenter", "-t", pid, "-n", "ip", "route", "add", "default", "via", bridgeIP)

	fmt.Printf("[veth]  %s → %s via %s\n", ctrIP, bridgeName, bridgeIP)
}

// setupNAT 确保共享子网能出公网。规则只加一次，多个容器复用。
func setupNAT() {
	must(os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644))

	if _, err := os.Stat("/tmp/mdbr0-nat-ready"); err == nil {
		return
	}
	runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", bridgeSubnet, "-j", "MASQUERADE")
	os.WriteFile("/tmp/mdbr0-nat-ready", []byte("1"), 0644)
	fmt.Printf("[nat]   MASQUERADE %s → internet\n", bridgeSubnet)
}

func cleanupVeth(vethHost string) {
	runCmdSilent("ip", "link", "del", vethHost)
}

func waitForNetwork() {
	f := os.NewFile(3, "syncpipe")
	buf := make([]byte, 1)
	_, err := f.Read(buf)
	must(err)
	f.Close()
	fmt.Printf("[net]   network ready\n")
}

func setupCgroup(name string) {
	cg := "/sys/fs/cgroup/" + name
	must(os.MkdirAll(cg, 0755))

	must(os.WriteFile(cg+"/cpu.max", []byte("100000 100000"), 0644))
	must(os.WriteFile(cg+"/memory.max", []byte("134217728"), 0644))
	must(os.WriteFile(cg+"/cgroup.procs", []byte(strconv.Itoa(os.Getpid())), 0644))

	fmt.Printf("[cgroup] limits set: 1 CPU, 128MB memory\n")
}

func cleanupCgroup(name string) {
	cg := "/sys/fs/cgroup/" + name
	os.WriteFile(cg+"/cgroup.kill", []byte("1"), 0644)
	os.Remove(cg)
	fmt.Printf("[cgroup] cleaned up\n")
}

// safeRemoveAll 是 os.RemoveAll 的一层保险。
// 就算上面的 MS_PRIVATE 隔离出于某种原因失效（比如以后改了挂载逻辑忘记加），
// 这里也会在真正删除前检查每个子路径的设备号（st_dev），
// 一旦发现和 overlayBase 本身不是同一个设备，直接拒绝删除，
// 而不是像 os.RemoveAll 那样无视挂载边界、一路删穿到别的文件系统上。
func safeRemoveAll(path string) {
	rootFi, err := os.Lstat(path)
	if err != nil {
		return // 目录已经不在了，没什么好删的
	}
	rootDev := rootFi.Sys().(*syscall.Stat_t).Dev

	err = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Dev != rootDev {
			return fmt.Errorf("safeRemoveAll: %s 跨越了挂载边界（dev %d != %d），拒绝删除", p, st.Dev, rootDev)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[overlay] 清理中止，未删除任何内容: %v\n", err)
		return
	}

	if err := os.RemoveAll(path); err != nil {
		fmt.Fprintf(os.Stderr, "[overlay] 清理 %s 失败: %v\n", path, err)
		return
	}
	fmt.Printf("[overlay] cleaned up %s\n", path)
}

func runCmd(args ...string) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stderr = os.Stderr
	must(cmd.Run())
}

func runCmdSilent(args ...string) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Run()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
