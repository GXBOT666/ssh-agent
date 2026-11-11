package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ============================================================================
// 巡检输出解析
//
// 这些解析器处理常见的 Linux 命令输出：
//   df -P -B1  -> 磁盘使用率
//   cat /proc/meminfo -> 内存 / Swap 使用率
//   cat /proc/loadavg -> 负载
// 解析失败不会让任务失败，只在 detail 中说明。
// ============================================================================

// DiskUsage 是单个挂载点的磁盘使用情况。
type DiskUsage struct {
	Mount      string
	UsedPct    float64
	Used, Size int64
}

// applyInspect 解析命令输出并写入 res.Detail / res.Metrics，超过阈值时降级为 warn。
func applyInspect(cfg *Config, t *Task, res *TaskResult, host string) {
	if t.Inspect == nil {
		return
	}
	a := cfg.Alert
	if res.Metrics == nil {
		res.Metrics = map[string]float64{}
	}
	var alerts []string

	switch t.Inspect.Kind {
	case "df":
		disks := parseDF(res.Stdout)
		if len(disks) == 0 {
			res.Detail = "未能解析 df 输出"
			return
		}
		want := t.Inspect.Mounts
		var parts []string
		var max float64
		var maxMount string
		for _, d := range disks {
			if len(want) > 0 && !containsFold(want, d.Mount) {
				continue
			}
			res.Metrics["disk_"+d.Mount] = d.UsedPct
			parts = append(parts, fmt.Sprintf("%s %.0f%%", d.Mount, d.UsedPct))
			if d.UsedPct > max {
				max, maxMount = d.UsedPct, d.Mount
			}
			if d.UsedPct >= a.DiskUsedPercent {
				alerts = append(alerts, fmt.Sprintf("磁盘 %s 使用率 %.0f%% ≥ 阈值 %.0f%%", d.Mount, d.UsedPct, a.DiskUsedPercent))
			}
		}
		res.Metrics["disk_max"] = max
		if maxMount != "" {
			res.Detail = fmt.Sprintf("最高 %s %.0f%%", maxMount, max)
			if len(alerts) == 0 {
				res.Detail += "（" + strings.Join(parts, ", ") + "）"
			}
		} else {
			res.Detail = strings.Join(parts, ", ")
		}
		// 超过阈值的挂载点很多时只列前几条，避免刷屏
		if len(alerts) > 3 {
			more := len(alerts) - 3
			alerts = alerts[:3]
			alerts = append(alerts, fmt.Sprintf("另有 %d 个挂载点超阈值（可用 inspect.mounts 只关注关键目录）", more))
		}

	case "meminfo":
		memUsed, swapUsed, err := parseMemInfo(res.Stdout)
		if err != nil {
			res.Detail = "未能解析 /proc/meminfo: " + err.Error()
			return
		}
		res.Metrics["mem_used"] = memUsed
		res.Metrics["swap_used"] = swapUsed
		res.Detail = fmt.Sprintf("内存 %.1f%%", memUsed)
		if swapUsed > 0 || a.SwapUsedPercent > 0 {
			res.Detail += fmt.Sprintf("，Swap %.1f%%", swapUsed)
		}
		if memUsed >= a.MemUsedPercent {
			alerts = append(alerts, fmt.Sprintf("内存使用率 %.1f%% ≥ 阈值 %.0f%%", memUsed, a.MemUsedPercent))
		}
		if swapUsed >= a.SwapUsedPercent {
			alerts = append(alerts, fmt.Sprintf("Swap 使用率 %.1f%% ≥ 阈值 %.0f%%", swapUsed, a.SwapUsedPercent))
		}

	case "load":
		load1, cores, err := parseLoadAvg(res.Stdout)
		if err != nil {
			res.Detail = "未能解析负载: " + err.Error()
			return
		}
		perCore := 0.0
		if cores > 0 {
			perCore = load1 / float64(cores)
		}
		res.Metrics["load1"] = load1
		res.Metrics["cores"] = float64(cores)
		res.Detail = fmt.Sprintf("负载(1m) %.2f，%d 核，每核 %.2f", load1, cores, perCore)
		if perCore >= a.LoadPerCore {
			alerts = append(alerts, fmt.Sprintf("1 分钟负载/核 %.2f ≥ 阈值 %.2f", perCore, a.LoadPerCore))
		}
	}

	if len(alerts) > 0 {
		if res.Status == StatusOK {
			res.Status = StatusWarn
		}
		res.Alerts = append(res.Alerts, alerts...)
	}
}

// parseDF 解析 df 输出，兼容三种常见格式：
//
//	Linux : Filesystem 1B-blocks Used Available Use% Mounted on   (df -P -B1)
//	macOS : Filesystem 1024-blocks Used Available Capacity Mounted on (df -P -k)
//	BusyBox: Filesystem 1K-blocks Used Available Use% Mounted on
//
// 单位从表头推断；无法推断时按 1K 估算（只影响显示，不影响百分比）。
func parseDF(out string) []DiskUsage {
	var res []DiskUsage
	unit := int64(1024)
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if i == 0 && strings.Contains(line, "Filesystem") {
			unit = dfUnit(line)
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 && strings.Contains(line, "Filesystem") {
			continue
		}
		// df -P 的挂载点是最后一列，文件系统名不含空格；按空格切即可
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		pct := strings.TrimSuffix(f[len(f)-2], "%")
		usedPct, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			continue
		}
		usedBlocks, _ := strconv.ParseInt(f[len(f)-4], 10, 64)
		sizeBlocks, _ := strconv.ParseInt(f[len(f)-5], 10, 64)
		res = append(res, DiskUsage{
			Mount: f[len(f)-1], UsedPct: usedPct,
			Used: usedBlocks * unit, Size: sizeBlocks * unit,
		})
	}
	// 过滤掉特殊文件系统（tmpfs/devtmpfs 也保留，但使用率为 0 时排后面）
	sort.SliceStable(res, func(i, j int) bool { return res[i].UsedPct > res[j].UsedPct })
	return res
}

// dfUnit 从表头推断块大小（字节）。
func dfUnit(header string) int64 {
	h := strings.ToLower(header)
	fields := strings.Fields(h)
	for _, f := range fields {
		if !strings.Contains(f, "block") {
			continue
		}
		// 形如 1b-blocks / 512-blocks / 1024-blocks / 1k-blocks
		prefix := strings.TrimSuffix(strings.SplitN(f, "-", 2)[0], "blocks")
		prefix = strings.TrimSuffix(prefix, "-")
		num, suf := prefix, byte(0)
		if len(prefix) > 0 {
			last := prefix[len(prefix)-1]
			if last == 'k' || last == 'b' || last == 'm' || last == 'g' {
				num, suf = prefix[:len(prefix)-1], last
			}
		}
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil || n <= 0 {
			n = 1
		}
		switch suf {
		case 'k':
			return n * 1024
		case 'm':
			return n * 1024 * 1024
		case 'g':
			return n * 1024 * 1024 * 1024
		default:
			return n
		}
	}
	return 1024
}

// parseMemInfo 返回内存使用率与 Swap 使用率（百分比）。
func parseMemInfo(out string) (memPct, swapPct float64, err error) {
	vals := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, e := strconv.ParseFloat(fields[1], 64)
		if e != nil {
			continue
		}
		vals[strings.TrimSuffix(fields[0], ":")] = v
	}
	total, ok := vals["MemTotal"]
	if !ok || total <= 0 {
		return 0, 0, fmt.Errorf("缺少 MemTotal")
	}
	// 可用内存优先用 MemAvailable，旧内核退化为 free+buffers+cached
	avail, hasAvail := vals["MemAvailable"]
	if !hasAvail {
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	memPct = (total - avail) / total * 100
	if memPct < 0 {
		memPct = 0
	}
	if st, ok := vals["SwapTotal"]; ok && st > 0 {
		sf := vals["SwapFree"]
		swapPct = (st - sf) / st * 100
		if swapPct < 0 {
			swapPct = 0
		}
	}
	return memPct, swapPct, nil
}

// parseLoadAvg 解析 loadavg 与 CPU 核数。
// 期望输入: "0.50 0.40 0.30 1/200 12345\n8"
func parseLoadAvg(out string) (load1 float64, cores int, err error) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && strings.Contains(f[3], "/") {
			load1, err = strconv.ParseFloat(f[0], 64)
			if err != nil {
				return 0, 0, err
			}
			continue
		}
		if len(f) == 1 {
			if n, e := strconv.Atoi(f[0]); e == nil && n > 0 {
				cores = n
			}
		}
	}
	if cores == 0 {
		return 0, 0, fmt.Errorf("缺少 CPU 核数")
	}
	return load1, cores, nil
}
