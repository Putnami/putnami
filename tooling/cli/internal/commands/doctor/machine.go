package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/output"
)

// LoadSample is a read-only measurement; unavailable hosts report Known=false.
type LoadSample struct {
	Known     bool    `json:"known"`
	OneMinute float64 `json:"oneMinute,omitempty"`
	CPUs      int     `json:"cpus,omitempty"`
	Ratio     float64 `json:"ratio,omitempty"`
}

// LoadReader makes the platform inputs replaceable without changing the
// production sampling path.
type LoadReader struct {
	OS       string
	ReadFile func(string) ([]byte, error)
	Command  func(string, ...string) ([]byte, error)
	NumCPU   func() int
}

func NativeLoadReader() LoadReader {
	return LoadReader{OS: runtime.GOOS, ReadFile: os.ReadFile,
		Command: func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).Output() },
		NumCPU:  runtime.NumCPU}
}

// MachineLoad returns unknown when the host cannot provide a one-minute load
// average or a positive logical CPU count.
func MachineLoad(reader LoadReader) LoadSample {
	if reader.NumCPU == nil {
		return LoadSample{}
	}
	cpus := reader.NumCPU()
	if cpus < 1 {
		return LoadSample{}
	}
	var raw string
	switch reader.OS {
	case "linux":
		if reader.ReadFile == nil {
			return LoadSample{}
		}
		data, err := reader.ReadFile("/proc/loadavg")
		if err != nil {
			return LoadSample{}
		}
		raw = string(data)
	case "darwin":
		if reader.Command == nil {
			return LoadSample{}
		}
		data, err := reader.Command("/usr/sbin/sysctl", "-n", "vm.loadavg")
		if err != nil {
			return LoadSample{}
		}
		raw = strings.Trim(string(data), "{} \n\t")
	default:
		return LoadSample{}
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return LoadSample{}
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || load < 0 {
		return LoadSample{}
	}
	return LoadSample{Known: true, OneMinute: load, CPUs: cpus, Ratio: load / float64(cpus)}
}

func (s LoadSample) String() string {
	if !s.Known {
		return "unknown"
	}
	return fmt.Sprintf("%.2f", s.Ratio)
}

// MachineCommand is `putnami doctor --machine`: it samples the machine load and
// prints it, as JSON under structured output and as the bare ratio (or
// "unknown") otherwise.
func MachineCommand(outputFormat string, reader LoadReader) error {
	return WriteMachineLoad(iox.Stdout(), outputFormat, MachineLoad(reader))
}

// WriteMachineLoad prints one sample to out.
func WriteMachineLoad(out io.Writer, outputFormat string, sample LoadSample) error {
	if output.StructuredOutput(outputFormat) {
		data, _ := json.Marshal(sample)
		iox.Fprintln(out, string(data))
	} else {
		iox.Fprintln(out, sample.String())
	}
	return nil
}
