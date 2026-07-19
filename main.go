package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

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
		args = []string{"/bin/sh"}
	}

	hostPID := os.Getpid()
	fmt.Printf("[run]   Running %v as PID %d\n", args, hostPID)

	overlayBase := fmt.Sprintf("/tmp/maingo-%d", hostPID)
	upperDir := overlayBase + "/upper"
	workDir := overlayBase + "/work"
	mergedDir := overlayBase + "/merged"
	vethHost := fmt.Sprintf("veth0-%d", hostPID)
	vethCtr := fmt.Sprintf("veth1-%d", hostPID)
	cgroupName := fmt.Sprintf("maingo-%d", hostPID)

	ipSuffix := hostPID % 253
	hostIP := fmt.Sprintf("10.0.%d.1", ipSuffix)
	ctrIP := fmt.Sprintf("10.0.%d.2", ipSuffix)
	subnet := fmt.Sprintf("10.0.%d.0/24", ipSuffix)

	must(os.MkdirAll(upperDir, 0755))
	must(os.MkdirAll(workDir, 0755))
	must(os.MkdirAll(mergedDir, 0755))

	// 用 defer 而不是"cmd.Wait() 之后顺序执行"：
	// must(cmd.Wait()) 在子进程非 0 退出时会 panic,
	// 之前的写法一旦这里 panic，后面的清理代码一行都不会跑，
	// 会在宿主机上留下孤儿 veth / iptables 规则 / cgroup / 临时目录。
	// defer 保证不管中间从哪里 panic，清理都会按 LIFO 顺序跑完。
	defer safeRemoveAll(overlayBase)
	defer cleanupCgroup(cgroupName)
	defer cleanupVeth(vethHost)
	defer cleanupNAT(subnet)

	r, w, err := os.Pipe()
	must(err)

	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{r}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWNET,
	}
	cmd.Env = append(os.Environ(),
		"MAINGO_MERGED="+mergedDir,
		"MAINGO_UPPER="+upperDir,
		"MAINGO_WORK="+workDir,
		"MAINGO_CGROUP="+cgroupName)

	must(cmd.Start())
	r.Close()

	setupVeth(cmd.Process.Pid, vethHost, vethCtr, hostIP, ctrIP, subnet)
	setupNAT(cmd.Process.Pid, ctrIP, subnet)

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

	lowerDir := "/home/mingdax/alpine-rootfs"
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

func setupVeth(childPID int, vethHost, vethCtr, hostIP, ctrIP, subnet string) {
	pid := strconv.Itoa(childPID)

	runCmdSilent("ip", "link", "del", vethHost)

	runCmd("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethCtr)
	runCmd("ip", "link", "set", vethCtr, "netns", pid)

	runCmd("ip", "addr", "add", hostIP+"/24", "dev", vethHost)
	runCmd("ip", "link", "set", vethHost, "up")

	runCmd("nsenter", "-t", pid, "-n", "ip", "addr", "add", ctrIP+"/24", "dev", vethCtr)
	runCmd("nsenter", "-t", pid, "-n", "ip", "link", "set", vethCtr, "up")
	runCmd("nsenter", "-t", pid, "-n", "ip", "link", "set", "lo", "up")

	fmt.Printf("[veth]  pair ready: host=%s, container=%s\n", hostIP, ctrIP)
}

func setupNAT(childPID int, ctrIP, subnet string) {
	pid := strconv.Itoa(childPID)

	must(os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644))
	runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-j", "MASQUERADE")
	runCmd("nsenter", "-t", pid, "-n", "ip", "route", "add", "default", "via",
		ctrIP[:len(ctrIP)-1]+"1")

	fmt.Printf("[nat]   NAT enabled: %s → internet\n", subnet)
}

func cleanupVeth(vethHost string) {
	runCmdSilent("ip", "link", "del", vethHost)
}

func cleanupNAT(subnet string) {
	runCmdSilent("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet, "-j",
		"MASQUERADE")
	fmt.Printf("[nat]   cleaned up\n")
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
