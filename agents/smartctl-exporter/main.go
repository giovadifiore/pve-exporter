package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type smartctlOutput struct {
	Device struct {
		Name     string `json:"name"`
		InfoName string `json:"info_name"`
		Protocol string `json:"protocol"`
		Type     string `json:"type"`
	} `json:"device"`
	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`
	SmartStatus  struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	NVMeSmartHealthInformationLog struct {
		Temperature    *float64 `json:"temperature"`
		PowerOnHours   *float64 `json:"power_on_hours"`
		PercentageUsed *float64 `json:"percentage_used"`
		DataUnitsWritten *float64 `json:"data_units_written"`
		AvailableSpare *float64 `json:"available_spare"`
	} `json:"nvme_smart_health_information_log"`
	PowerOnTime struct {
		Hours *float64 `json:"hours"`
	} `json:"power_on_time"`
	Temperature struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
	ATASmartAttributes struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value *float64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

type label struct {
	key   string
	value string
}

type exporter struct {
	smartctlBin string
	timeout     time.Duration
}

type jsonMetricsResponse struct {
	Disk                   string         `json:"disk"`
	Labels                 map[string]string `json:"labels"`
	SmartctlUp             int            `json:"smartctl_up"`
	SmartctlCommandExit    int            `json:"smartctl_command_exit_status"`
	DiskHealthy            *bool          `json:"smartctl_disk_healthy,omitempty"`
	TemperatureCelsius     *float64       `json:"smartctl_disk_temperature_celsius,omitempty"`
	PowerOnHours           *float64       `json:"smartctl_disk_power_on_hours,omitempty"`
	PercentageUsed         *float64       `json:"smartctl_disk_percentage_used,omitempty"`
	AvailableSparePercent  *float64       `json:"smartctl_disk_available_spare_percent,omitempty"`
	DataWrittenBytes       *float64       `json:"smartctl_disk_data_written_bytes,omitempty"`
	Stderr                 string         `json:"stderr,omitempty"`
}

var diskNamePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

func main() {
	listenAddress := flag.String("listen-address", ":9634", "HTTP listen address")
	metricsPath := flag.String("metrics-path", "/metrics", "HTTP path exposing metrics")
	smartctlBin := flag.String("smartctl-bin", "/usr/sbin/smartctl", "Path to smartctl binary")
	timeout := flag.Duration("timeout", 10*time.Second, "Timeout for each smartctl command execution")
	flag.Parse()

	exp := &exporter{smartctlBin: *smartctlBin, timeout: *timeout}

	mux := http.NewServeMux()
	mux.HandleFunc(*metricsPath, exp.metricsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/", rootHandler(*metricsPath))

	srv := &http.Server{
		Addr:              *listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("starting smartctl exporter on %s (metrics path %s)", *listenAddress, *metricsPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func (e *exporter) metricsHandler(w http.ResponseWriter, r *http.Request) {
	diskParam := strings.TrimSpace(r.URL.Query().Get("disk"))
	if diskParam == "" {
		http.Error(w, "missing 'disk' query parameter (example: /metrics?disk=sda)", http.StatusBadRequest)
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	if format != "prometheus" && format != "json" {
		http.Error(w, "invalid 'format' query parameter (supported values: prometheus, json)", http.StatusBadRequest)
		return
	}

	disk, err := normalizeDisk(diskParam)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid disk parameter: %v", err), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), e.timeout)
	defer cancel()

	out, exitCode, stderrText, err := runSmartctl(ctx, e.smartctlBin, disk)
	if err != nil {
		http.Error(w, fmt.Sprintf("smartctl error: %v", err), http.StatusBadGateway)
		return
	}

	labels := []label{
		{key: "disk", value: diskParam},
		{key: "device", value: firstNonEmpty(out.Device.Name, disk)},
		{key: "model", value: firstNonEmpty(out.ModelName, "unknown")},
		{key: "serial", value: firstNonEmpty(out.SerialNumber, "unknown")},
		{key: "type", value: detectDiskType(out)},
		{key: "protocol", value: firstNonEmpty(out.Device.Protocol, "unknown")},
	}

	if format == "json" {
		response := jsonMetricsResponse{
			Disk:                diskParam,
			Labels:              labelsToMap(labels),
			SmartctlUp:          1,
			SmartctlCommandExit: exitCode,
			Stderr:              stderrText,
		}

		if out.SmartStatus.Passed != nil {
			response.DiskHealthy = out.SmartStatus.Passed
		}
		if temp, ok := extractTemperature(out); ok {
			response.TemperatureCelsius = &temp
		}
		if hours, ok := extractPowerOnHours(out); ok {
			response.PowerOnHours = &hours
		}
		if out.NVMeSmartHealthInformationLog.PercentageUsed != nil {
			response.PercentageUsed = out.NVMeSmartHealthInformationLog.PercentageUsed
		}
		if out.NVMeSmartHealthInformationLog.AvailableSpare != nil {
			response.AvailableSparePercent = out.NVMeSmartHealthInformationLog.AvailableSpare
		}
		if out.NVMeSmartHealthInformationLog.DataUnitsWritten != nil {
			bytesWritten := *out.NVMeSmartHealthInformationLog.DataUnitsWritten * 512000
			response.DataWrittenBytes = &bytesWritten
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			http.Error(w, fmt.Sprintf("failed to encode JSON response: %v", err), http.StatusInternalServerError)
		}
		return
	}

	var b strings.Builder
	b.Grow(2048)

	b.WriteString("# HELP smartctl_up smartctl scrape success for the selected disk.\n")
	b.WriteString("# TYPE smartctl_up gauge\n")
	writeMetric(&b, "smartctl_up", 1, labels...)

	b.WriteString("# HELP smartctl_command_exit_status smartctl process exit status for the selected disk.\n")
	b.WriteString("# TYPE smartctl_command_exit_status gauge\n")
	writeMetric(&b, "smartctl_command_exit_status", float64(exitCode), labels...)

	b.WriteString("# HELP smartctl_disk_info Static disk metadata.\n")
	b.WriteString("# TYPE smartctl_disk_info gauge\n")
	writeMetric(&b, "smartctl_disk_info", 1, labels...)

	if out.SmartStatus.Passed != nil {
		b.WriteString("# HELP smartctl_disk_healthy SMART overall-health self-assessment result (1=passed).\n")
		b.WriteString("# TYPE smartctl_disk_healthy gauge\n")
		if *out.SmartStatus.Passed {
			writeMetric(&b, "smartctl_disk_healthy", 1, labels...)
		} else {
			writeMetric(&b, "smartctl_disk_healthy", 0, labels...)
		}
	}

	if temp, ok := extractTemperature(out); ok {
		b.WriteString("# HELP smartctl_disk_temperature_celsius Disk temperature in Celsius.\n")
		b.WriteString("# TYPE smartctl_disk_temperature_celsius gauge\n")
		writeMetric(&b, "smartctl_disk_temperature_celsius", temp, labels...)
	}

	if hours, ok := extractPowerOnHours(out); ok {
		b.WriteString("# HELP smartctl_disk_power_on_hours Disk power-on hours.\n")
		b.WriteString("# TYPE smartctl_disk_power_on_hours gauge\n")
		writeMetric(&b, "smartctl_disk_power_on_hours", hours, labels...)
	}

	if out.NVMeSmartHealthInformationLog.PercentageUsed != nil {
		b.WriteString("# HELP smartctl_disk_percentage_used NVMe percentage used.\n")
		b.WriteString("# TYPE smartctl_disk_percentage_used gauge\n")
		writeMetric(&b, "smartctl_disk_percentage_used", *out.NVMeSmartHealthInformationLog.PercentageUsed, labels...)
	}

	if out.NVMeSmartHealthInformationLog.AvailableSpare != nil {
		b.WriteString("# HELP smartctl_disk_available_spare_percent NVMe available spare percentage.\n")
		b.WriteString("# TYPE smartctl_disk_available_spare_percent gauge\n")
		writeMetric(&b, "smartctl_disk_available_spare_percent", *out.NVMeSmartHealthInformationLog.AvailableSpare, labels...)
	}

	if out.NVMeSmartHealthInformationLog.DataUnitsWritten != nil {
		b.WriteString("# HELP smartctl_disk_data_written_bytes Total bytes written (NVMe data units written * 512000).\n")
		b.WriteString("# TYPE smartctl_disk_data_written_bytes gauge\n")
		writeMetric(&b, "smartctl_disk_data_written_bytes", *out.NVMeSmartHealthInformationLog.DataUnitsWritten*512000, labels...)
	}

	if stderrText != "" {
		stderrLabels := append(labels, label{key: "stderr", value: truncate(stderrText, 180)})
		b.WriteString("# HELP smartctl_stderr_present Whether smartctl wrote to stderr (1=yes).\n")
		b.WriteString("# TYPE smartctl_stderr_present gauge\n")
		writeMetric(&b, "smartctl_stderr_present", 1, stderrLabels...)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func runSmartctl(ctx context.Context, binaryPath string, disk string) (*smartctlOutput, int, string, error) {
	// Always include -a so smartctl returns the full set of available values.
	args := []string{"-j", "-a", disk}
	cmd := exec.CommandContext(ctx, binaryPath, args...)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, 0, stderr.String(), fmt.Errorf("failed to execute smartctl: %w", err)
		}
	}

	if ctx.Err() != nil {
		return nil, exitCode, stderr.String(), fmt.Errorf("smartctl execution timed out: %w", ctx.Err())
	}

	if stdout.Len() == 0 {
		return nil, exitCode, stderr.String(), fmt.Errorf("smartctl returned empty output")
	}

	var out smartctlOutput
	if unmarshalErr := json.Unmarshal(stdout.Bytes(), &out); unmarshalErr != nil {
		return nil, exitCode, stderr.String(), fmt.Errorf("invalid smartctl JSON output: %w", unmarshalErr)
	}

	return &out, exitCode, strings.TrimSpace(stderr.String()), nil
}

func normalizeDisk(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", errors.New("disk is empty")
	}

	if strings.HasPrefix(trimmed, "/dev/") {
		cleaned := path.Clean(trimmed)
		if !strings.HasPrefix(cleaned, "/dev/") {
			return "", errors.New("disk path must stay under /dev")
		}
		return cleaned, nil
	}

	if !diskNamePattern.MatchString(trimmed) {
		return "", errors.New("disk name can only contain letters, digits, dot, underscore, colon, dash")
	}

	return "/dev/" + trimmed, nil
}

func detectDiskType(out *smartctlOutput) string {
	if strings.EqualFold(out.Device.Protocol, "NVMe") {
		return "nvme"
	}
	if strings.EqualFold(out.Device.Protocol, "ATA") {
		return "sata"
	}
	if out.Device.Type != "" {
		return strings.ToLower(out.Device.Type)
	}
	return "unknown"
}

func extractPowerOnHours(out *smartctlOutput) (float64, bool) {
	if out.NVMeSmartHealthInformationLog.PowerOnHours != nil {
		return *out.NVMeSmartHealthInformationLog.PowerOnHours, true
	}
	if out.PowerOnTime.Hours != nil {
		return *out.PowerOnTime.Hours, true
	}
	return 0, false
}

func extractTemperature(out *smartctlOutput) (float64, bool) {
	if out.NVMeSmartHealthInformationLog.Temperature != nil {
		return *out.NVMeSmartHealthInformationLog.Temperature, true
	}
	if out.Temperature.Current != nil {
		return *out.Temperature.Current, true
	}
	for _, attr := range out.ATASmartAttributes.Table {
		if attr.ID == 194 && attr.Raw.Value != nil {
			return *attr.Raw.Value, true
		}
	}
	return 0, false
}

func writeMetric(b *strings.Builder, name string, value float64, labels ...label) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.key)
			b.WriteString("=\"")
			b.WriteString(escapeLabelValue(l.value))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	if math.IsNaN(value) || math.IsInf(value, 0) {
		b.WriteString("0")
	} else {
		b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	}
	b.WriteByte('\n')
}

func escapeLabelValue(v string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\n", "\\n",
		"\"", "\\\"",
	)
	return replacer.Replace(v)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func labelsToMap(labels []label) map[string]string {
	m := make(map[string]string, len(labels))
	for _, l := range labels {
		m[l.key] = l.value
	}
	return m
}

func truncate(value string, maxLen int) string {
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK\n"))
}

func rootHandler(metricsPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("smartctl exporter\n"))
		_, _ = w.Write([]byte("usage: " + metricsPath + "?disk=sda&format=json\n"))
		_, _ = w.Write([]byte("or:    " + metricsPath + "?disk=/dev/nvme0n1&format=prometheus\n"))
	}
}
