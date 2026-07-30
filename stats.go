package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// systemStats is a live snapshot of load and capacity: CPU usage, GPU
// utilization and memory, and disk usage, as opposed to system_info's static
// facts about the platform. Call it again and the numbers change.
type systemStats struct {
	CPU  cpuStats   `json:"cpu"`
	GPUs []gpuStats `json:"gpus,omitempty"`
	Disk diskStats  `json:"disk"`
}

type cpuStats struct {
	Model            string   `json:"model,omitempty"`
	LogicalCores     int      `json:"logical_cores"`
	PhysicalCores    int      `json:"physical_cores,omitempty"`
	UsagePercent     *float64 `json:"usage_percent,omitempty"`
	LoadAverage1Min  *float64 `json:"load_average_1min,omitempty"`
	LoadAverage5Min  *float64 `json:"load_average_5min,omitempty"`
	LoadAverage15Min *float64 `json:"load_average_15min,omitempty"`
	// Features lists instruction-set extensions (e.g. avx2, sse4_2, neon),
	// when the platform makes them easy to enumerate. Windows exposes no
	// simple inventory of these and is left empty.
	Features []string `json:"features,omitempty"`
}

type gpuStats struct {
	Name          string `json:"name"`
	MemoryTotalMB int64  `json:"memory_total_mb,omitempty"`
	// MemoryUsedMB and UtilizationPercent are only ever populated by
	// nvidia-smi; the platform inventories used as a fallback elsewhere
	// (Windows' WMI, macOS' system_profiler, Linux's lspci) report a GPU's
	// name and sometimes its memory size, but not what it is doing right now.
	MemoryUsedMB       *int64   `json:"memory_used_mb,omitempty"`
	UtilizationPercent *float64 `json:"utilization_percent,omitempty"`
}

type diskStats struct {
	Path        string  `json:"path"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// registerStatsTools adds the tool that reports live utilization, as opposed
// to registerSystemTools' static facts about the machine.
func (s *Server) registerStatsTools() {
	s.RegisterTool(Tool{
		Name:  "system_stats",
		Title: "CPU, GPU and disk usage",
		Description: "Report a live snapshot of this machine: CPU usage percent, core counts and " +
			"instruction-set features; GPU name, memory size, and utilization when a GPU and its driver " +
			"tools are found; and the usage and total size of the disk the workspace lives on. Unlike " +
			"system_info, which is static, this changes call to call - use it to check whether a build, a " +
			"model or a large job actually has the headroom it needs, or is about to run out of disk. GPU " +
			"and some CPU capability fields are left out where the platform does not make them easy to " +
			"determine without a driver-specific tool (nvidia-smi and the like); that is normal, not a failure.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schema(nil, nil),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		stats := gatherSystemStats(ctx, s.workspace().Root)
		return &CallToolResult{
			Content:           textContent(stats.summarize()),
			StructuredContent: stats,
		}, nil
	})
}

// gatherSystemStats collects everything system_stats reports, and is what the
// console agent's /stats command calls directly. Every probe is best-effort
// and platform-dependent; one that fails or does not apply here simply
// leaves its fields empty rather than failing the whole call.
func gatherSystemStats(ctx context.Context, workspaceRoot string) systemStats {
	return systemStats{
		CPU:  cpuStatsSnapshot(ctx),
		GPUs: gpuStatsSnapshot(ctx),
		Disk: diskStatsSnapshot(workspaceRoot),
	}
}

// runCapture runs a program with a timeout and returns its stdout, discarding
// the error: every caller here treats "the tool isn't installed" and "the
// tool failed" the same way, by reporting less rather than failing.
func runCapture(ctx context.Context, timeout time.Duration, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// --- CPU ---

func cpuStatsSnapshot(ctx context.Context) cpuStats {
	stats := cpuStats{LogicalCores: runtime.NumCPU()}
	switch runtime.GOOS {
	case "linux":
		linuxCPUInfo(&stats)
		if usage, ok := linuxCPUUsage(ctx); ok {
			stats.UsagePercent = &usage
		}
		if l1, l5, l15, ok := linuxLoadAverage(); ok {
			stats.LoadAverage1Min, stats.LoadAverage5Min, stats.LoadAverage15Min = &l1, &l5, &l15
		}
	case "darwin":
		darwinCPUInfo(ctx, &stats)
		if usage, ok := darwinCPUUsage(ctx); ok {
			stats.UsagePercent = &usage
		}
		if l1, l5, l15, ok := darwinLoadAverage(ctx); ok {
			stats.LoadAverage1Min, stats.LoadAverage5Min, stats.LoadAverage15Min = &l1, &l5, &l15
		}
	case "windows":
		windowsCPUInfo(ctx, &stats)
	}
	return stats
}

// linuxCPUInfo reads the model name, instruction-set flags and physical core
// count out of /proc/cpuinfo, which lists one block per logical processor
// separated by a blank line.
func linuxCPUInfo(stats *cpuStats) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return
	}
	cores := map[string]bool{}
	for _, block := range strings.Split(string(data), "\n\n") {
		var physID, coreID string
		for _, line := range strings.Split(block, "\n") {
			name, value, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			name, value = strings.TrimSpace(name), strings.TrimSpace(value)
			switch name {
			case "model name":
				if stats.Model == "" {
					stats.Model = value
				}
			case "physical id":
				physID = value
			case "core id":
				coreID = value
			case "flags", "Features": // "Features" is what arm64 calls it
				if stats.Features == nil {
					stats.Features = strings.Fields(value)
				}
			}
		}
		if physID != "" && coreID != "" {
			cores[physID+"/"+coreID] = true
		}
	}
	if len(cores) > 0 {
		stats.PhysicalCores = len(cores)
	}
}

// linuxCPUUsage samples /proc/stat's aggregate cpu line twice, briefly, and
// reports the fraction of that window that was not idle. A single reading
// would only give a cumulative counter since boot, not a current percentage.
func linuxCPUUsage(ctx context.Context) (float64, bool) {
	idle1, total1, ok := readProcStat()
	if !ok {
		return 0, false
	}
	select {
	case <-ctx.Done():
		return 0, false
	case <-time.After(150 * time.Millisecond):
	}
	idle2, total2, ok := readProcStat()
	if !ok || total2 <= total1 {
		return 0, false
	}
	idleDelta, totalDelta := float64(idle2-idle1), float64(total2-total1)
	return (1 - idleDelta/totalDelta) * 100, true
}

func readProcStat() (idle, total uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var sum uint64
	for _, f := range fields[1:] {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		sum += n
	}
	idleTicks, _ := strconv.ParseUint(fields[4], 10, 64) // idle
	var iowait uint64
	if len(fields) > 5 {
		iowait, _ = strconv.ParseUint(fields[5], 10, 64) // iowait counts as idle too
	}
	return idleTicks + iowait, sum, true
}

func linuxLoadAverage() (l1, l5, l15 float64, ok bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	var e1, e2, e3 error
	l1, e1 = strconv.ParseFloat(fields[0], 64)
	l5, e2 = strconv.ParseFloat(fields[1], 64)
	l15, e3 = strconv.ParseFloat(fields[2], 64)
	return l1, l5, l15, e1 == nil && e2 == nil && e3 == nil
}

func darwinCPUInfo(ctx context.Context, stats *cpuStats) {
	if out, ok := runCapture(ctx, 5*time.Second, "sysctl", "-n", "machdep.cpu.brand_string"); ok {
		stats.Model = strings.TrimSpace(out)
	}
	if out, ok := runCapture(ctx, 5*time.Second, "sysctl", "-n", "hw.physicalcpu"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil {
			stats.PhysicalCores = n
		}
	}
	// machdep.cpu.features/leaf7_features are Intel-only; Apple Silicon has
	// no equivalent sysctl, so this simply finds nothing there and Features
	// stays empty, same as Windows.
	var features []string
	for _, key := range []string{"machdep.cpu.features", "machdep.cpu.leaf7_features"} {
		if out, ok := runCapture(ctx, 5*time.Second, "sysctl", "-n", key); ok {
			features = append(features, strings.Fields(strings.ToLower(strings.TrimSpace(out)))...)
		}
	}
	stats.Features = features
}

var darwinCPULine = regexp.MustCompile(`CPU usage:\s*[\d.]+%\s*user,\s*[\d.]+%\s*sys,\s*([\d.]+)%\s*idle`)

func darwinCPUUsage(ctx context.Context) (float64, bool) {
	out, ok := runCapture(ctx, 5*time.Second, "top", "-l", "1", "-n", "0")
	if !ok {
		return 0, false
	}
	m := darwinCPULine.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	idle, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return 100 - idle, true
}

var darwinLoadAvgNumber = regexp.MustCompile(`[\d.]+`)

func darwinLoadAverage(ctx context.Context) (l1, l5, l15 float64, ok bool) {
	out, got := runCapture(ctx, 5*time.Second, "sysctl", "-n", "vm.loadavg")
	if !got {
		return 0, 0, 0, false
	}
	nums := darwinLoadAvgNumber.FindAllString(out, -1)
	if len(nums) < 3 {
		return 0, 0, 0, false
	}
	var e1, e2, e3 error
	l1, e1 = strconv.ParseFloat(nums[0], 64)
	l5, e2 = strconv.ParseFloat(nums[1], 64)
	l15, e3 = strconv.ParseFloat(nums[2], 64)
	return l1, l5, l15, e1 == nil && e2 == nil && e3 == nil
}

// windowsCPUInfo runs one PowerShell query for everything WMI can say about
// the processor(s): name, core count and the OS's own periodically-updated
// load figure. A single-socket machine - almost everyone - gets one JSON
// object rather than an array, so both shapes are accepted.
func windowsCPUInfo(ctx context.Context, stats *cpuStats) {
	type cimProcessor struct {
		Name           string
		NumberOfCores  int
		LoadPercentage *int
	}
	out, ok := runCapture(ctx, 10*time.Second, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_Processor | Select-Object Name,NumberOfCores,LoadPercentage | ConvertTo-Json -Compress")
	if !ok {
		return
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return
	}
	var procs []cimProcessor
	if strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal([]byte(trimmed), &procs) != nil {
			return
		}
	} else {
		var one cimProcessor
		if json.Unmarshal([]byte(trimmed), &one) != nil {
			return
		}
		procs = []cimProcessor{one}
	}
	var totalCores, loadSum, loadCount int
	for _, p := range procs {
		if stats.Model == "" {
			stats.Model = p.Name
		}
		totalCores += p.NumberOfCores
		if p.LoadPercentage != nil {
			loadSum += *p.LoadPercentage
			loadCount++
		}
	}
	if totalCores > 0 {
		stats.PhysicalCores = totalCores
	}
	if loadCount > 0 {
		avg := float64(loadSum) / float64(loadCount)
		stats.UsagePercent = &avg
	}
}

// --- GPU ---

// gpuStatsSnapshot tries nvidia-smi first regardless of platform, since it
// is the only source here that reports memory used and utilization rather
// than just a name; everything else is a name-only (sometimes name+size)
// fallback for when there is no NVIDIA driver, or none at all.
func gpuStatsSnapshot(ctx context.Context) []gpuStats {
	if gpus, ok := nvidiaGPUStats(ctx); ok {
		return gpus
	}
	switch runtime.GOOS {
	case "windows":
		return windowsGPUStats(ctx)
	case "darwin":
		return darwinGPUStats(ctx)
	case "linux":
		return linuxGPUStats(ctx)
	}
	return nil
}

func nvidiaGPUStats(ctx context.Context) ([]gpuStats, bool) {
	out, ok := runCapture(ctx, 5*time.Second, "nvidia-smi",
		"--query-gpu=name,memory.total,memory.used,utilization.gpu", "--format=csv,noheader,nounits")
	if !ok {
		return nil, false
	}
	var gpus []gpuStats
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		gpu := gpuStats{Name: strings.TrimSpace(fields[0])}
		if v, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64); err == nil {
			gpu.MemoryTotalMB = v
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64); err == nil {
			gpu.MemoryUsedMB = &v
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(fields[3]), 64); err == nil {
			gpu.UtilizationPercent = &v
		}
		gpus = append(gpus, gpu)
	}
	return gpus, len(gpus) > 0
}

// windowsGPUStats falls back to WMI's video controller inventory, which has
// no NVIDIA driver dependency but also no live utilization.
func windowsGPUStats(ctx context.Context) []gpuStats {
	type cimVideo struct {
		Name       string
		AdapterRAM *int64
	}
	out, ok := runCapture(ctx, 10*time.Second, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_VideoController | Select-Object Name,AdapterRAM | ConvertTo-Json -Compress")
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	var adapters []cimVideo
	if strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal([]byte(trimmed), &adapters) != nil {
			return nil
		}
	} else {
		var one cimVideo
		if json.Unmarshal([]byte(trimmed), &one) != nil {
			return nil
		}
		adapters = []cimVideo{one}
	}
	var gpus []gpuStats
	for _, a := range adapters {
		gpu := gpuStats{Name: a.Name}
		// AdapterRAM is a signed 32-bit WMI field: it wraps to a small or
		// negative number for any card with 4GB+ of VRAM, which is most of
		// them, so an implausible value is left out rather than reported wrong.
		if a.AdapterRAM != nil && *a.AdapterRAM > 0 && *a.AdapterRAM < 1<<32 {
			gpu.MemoryTotalMB = *a.AdapterRAM / (1024 * 1024)
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

var (
	darwinGPUName = regexp.MustCompile(`Chipset Model:\s*(.+)`)
	darwinGPUVRAM = regexp.MustCompile(`VRAM \([^)]*\):\s*([\d.]+)\s*(MB|GB)`)
)

// darwinGPUStats parses `system_profiler`'s display report. There is a
// -json form, but the plain text is stable enough and one regexp simpler.
func darwinGPUStats(ctx context.Context) []gpuStats {
	out, ok := runCapture(ctx, 10*time.Second, "system_profiler", "SPDisplaysDataType")
	if !ok {
		return nil
	}
	names := darwinGPUName.FindAllStringSubmatch(out, -1)
	if len(names) == 0 {
		return nil
	}
	vram := darwinGPUVRAM.FindStringSubmatch(out)
	var gpus []gpuStats
	for i, m := range names {
		gpu := gpuStats{Name: strings.TrimSpace(m[1])}
		if i == 0 && vram != nil {
			if v, err := strconv.ParseFloat(vram[1], 64); err == nil {
				if vram[2] == "GB" {
					v *= 1024
				}
				gpu.MemoryTotalMB = int64(v)
			}
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

var linuxGPULine = regexp.MustCompile(`(?i)VGA compatible controller|3D controller`)

// linuxGPUStats is the last resort when there is no nvidia-smi: lspci names
// the card, with no memory size or utilization.
func linuxGPUStats(ctx context.Context) []gpuStats {
	out, ok := runCapture(ctx, 5*time.Second, "lspci")
	if !ok {
		return nil
	}
	var gpus []gpuStats
	for _, line := range strings.Split(out, "\n") {
		if !linuxGPULine.MatchString(line) {
			continue
		}
		_, name, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		gpus = append(gpus, gpuStats{Name: strings.TrimSpace(name)})
	}
	return gpus
}

// --- Disk ---

// diskStatsSnapshot reports usage for the volume the workspace lives on -
// the disk a build, a download or a large edit would actually run out of,
// rather than every mounted filesystem on the machine.
func diskStatsSnapshot(path string) diskStats {
	stats := diskStats{Path: path}
	total, free, err := diskUsage(path)
	if err != nil || total == 0 {
		return stats
	}
	stats.TotalBytes = total
	stats.FreeBytes = free
	stats.UsedBytes = total - free
	stats.UsedPercent = float64(stats.UsedBytes) / float64(total) * 100
	return stats
}

// summarize renders the snapshot as the text block of the tool result.
func (s systemStats) summarize() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CPU")
	if s.CPU.Model != "" {
		fmt.Fprintf(&b, "  %s", s.CPU.Model)
	}
	b.WriteString("\n")
	cores := fmt.Sprintf("%d logical", s.CPU.LogicalCores)
	if s.CPU.PhysicalCores > 0 {
		cores = fmt.Sprintf("%d physical / %s", s.CPU.PhysicalCores, cores)
	}
	fmt.Fprintf(&b, "  cores   %s\n", cores)
	if s.CPU.UsagePercent != nil {
		fmt.Fprintf(&b, "  usage   %.1f%%\n", *s.CPU.UsagePercent)
	}
	if s.CPU.LoadAverage1Min != nil {
		fmt.Fprintf(&b, "  load    %.2f %.2f %.2f (1/5/15 min)\n",
			*s.CPU.LoadAverage1Min, *s.CPU.LoadAverage5Min, *s.CPU.LoadAverage15Min)
	}
	if len(s.CPU.Features) > 0 {
		fmt.Fprintf(&b, "  features  %s\n", strings.Join(s.CPU.Features, " "))
	}

	if len(s.GPUs) > 0 {
		b.WriteString("\nGPU\n")
		for _, g := range s.GPUs {
			fmt.Fprintf(&b, "  %s", g.Name)
			if g.MemoryTotalMB > 0 {
				if g.MemoryUsedMB != nil {
					fmt.Fprintf(&b, "  %d / %d MB", *g.MemoryUsedMB, g.MemoryTotalMB)
				} else {
					fmt.Fprintf(&b, "  %d MB", g.MemoryTotalMB)
				}
			}
			if g.UtilizationPercent != nil {
				fmt.Fprintf(&b, "  %.0f%% util", *g.UtilizationPercent)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\nDisk\n")
	if s.Disk.TotalBytes > 0 {
		fmt.Fprintf(&b, "  %s\n", s.Disk.Path)
		fmt.Fprintf(&b, "  %s used / %s total (%.1f%%), %s free\n",
			formatBytes(s.Disk.UsedBytes), formatBytes(s.Disk.TotalBytes), s.Disk.UsedPercent, formatBytes(s.Disk.FreeBytes))
	} else {
		fmt.Fprintf(&b, "  %s: could not be determined\n", s.Disk.Path)
	}
	return b.String()
}

// formatBytes renders a byte count the way df -h or Explorer would, not as a
// raw integer a model would otherwise have to convert itself.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
