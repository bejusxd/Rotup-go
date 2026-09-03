//go:build linux

package linux

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// Enumeration uses lsblk's JSON output rather than blkid, sysfs walking, or a
// libudev binding.
//
// lsblk ships with util-linux on every supported distribution, needs no
// privilege for the fields ROTUP reads, and emits structured JSON, so there is
// no column-aligned human output to parse. sysfs walking would reimplement what
// lsblk already does correctly, and a libudev binding would pull cgo into the
// build for no additional information. Mount state is deliberately not taken
// from here: it comes from the kernel's own mount table, which is authoritative.
const (
	// enumerateTimeout bounds one enumeration.
	enumerateTimeout = 20 * time.Second
	// enumerateOutputLimit bounds the JSON ROTUP will parse.
	enumerateOutputLimit = 4 * 1024 * 1024
)

// enumerateColumns is pinned to long-standing lsblk columns so that the
// invocation does not fail on an older util-linux. MOUNTPOINT is deliberately
// absent: mount state comes from mountinfo.
var enumerateColumns = "NAME,PATH,TYPE,FSTYPE,UUID,LABEL,SIZE,RM,RO,TRAN,PKNAME"

// Enumerator implements ports.MediaEnumerator using lsblk.
type Enumerator struct {
	executablePath string
	runner         ports.ProcessRunner
}

// NewEnumerator builds the lsblk-backed enumerator. executablePath must be an
// explicit path; the inherited PATH is not consulted here.
func NewEnumerator(executablePath string, runner ports.ProcessRunner) *Enumerator {
	return &Enumerator{executablePath: executablePath, runner: runner}
}

// lsblkOutput mirrors the subset of lsblk's JSON that ROTUP reads.
type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name       string        `json:"name"`
	Path       string        `json:"path"`
	Type       string        `json:"type"`
	FSType     string        `json:"fstype"`
	UUID       string        `json:"uuid"`
	Label      string        `json:"label"`
	Size       int64         `json:"size"`
	Removable  boolish       `json:"rm"`
	ReadOnly   boolish       `json:"ro"`
	Transport  string        `json:"tran"`
	ParentName string        `json:"pkname"`
	Children   []lsblkDevice `json:"children"`
}

// boolish decodes the RM and RO columns, which lsblk has emitted as both JSON
// booleans and as "0"/"1" strings across versions.
type boolish bool

func (b *boolish) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch strings.ToLower(text) {
	case "1", "true":
		*b = true
	default:
		*b = false
	}
	return nil
}

// Enumerate returns every currently observed filesystem-bearing device.
func (e *Enumerator) Enumerate(ctx context.Context) ([]domain.ObservedMedia, error) {
	if strings.TrimSpace(e.executablePath) == "" {
		return nil, domain.NewError(domain.ErrClassConfiguration, "lsblk_unconfigured",
			"no lsblk executable is configured; set ROTUP_LSBLK_PATH")
	}
	runCtx, cancel := context.WithTimeout(ctx, enumerateTimeout)
	defer cancel()

	result := e.runner.Run(runCtx, ports.CommandSpec{
		Path:            e.executablePath,
		Args:            []string{"--json", "--bytes", "--paths", "--output", enumerateColumns},
		Env:             []string{},
		StdoutTailLimit: enumerateOutputLimit,
		StderrTailLimit: 32 * 1024,
	})

	switch {
	case result.TimedOut:
		return nil, domain.NewError(domain.ErrClassTimeout, "lsblk_timeout",
			"listing block devices took too long and was stopped")
	case result.Canceled:
		return nil, domain.NewError(domain.ErrClassCancellation, "lsblk_canceled",
			"listing block devices was canceled")
	case result.Err != nil:
		return nil, domain.WrapError(domain.ErrClassExternalTool, "lsblk_start_failed",
			"the lsblk executable could not be run", result.Err)
	case !result.Exited || result.ExitCode != 0:
		return nil, domain.NewError(domain.ErrClassExternalTool, "lsblk_failed",
			"listing block devices failed")
	case result.StdoutTruncated:
		// A truncated listing could silently hide a device, and a hidden device
		// is exactly how a duplicate UUID would go undetected.
		return nil, domain.NewError(domain.ErrClassExternalTool, "lsblk_output_truncated",
			"the block device listing was too large to read completely")
	}

	var parsed lsblkOutput
	if err := json.Unmarshal(result.Stdout, &parsed); err != nil {
		return nil, domain.WrapError(domain.ErrClassExternalTool, "lsblk_unparsed",
			"the block device listing could not be read", err)
	}

	observedAt := time.Now().UTC()
	var media []domain.ObservedMedia
	for _, device := range parsed.BlockDevices {
		media = append(media, flatten(device, "", observedAt)...)
	}
	return media, nil
}

// flatten walks the device tree and keeps every node that carries a filesystem.
// Both whole disks and partitions can carry one, so neither is assumed.
func flatten(device lsblkDevice, parentPath string, observedAt time.Time) []domain.ObservedMedia {
	var result []domain.ObservedMedia

	path := device.Path
	if path == "" && device.Name != "" {
		path = "/dev/" + strings.TrimPrefix(device.Name, "/dev/")
	}
	if device.FSType != "" {
		result = append(result, domain.ObservedMedia{
			DevicePath:     path,
			FilesystemUUID: domain.NormalizeFilesystemUUID(device.UUID),
			FilesystemType: domain.NormalizeFilesystemType(device.FSType),
			Label:          device.Label,
			SizeBytes:      device.Size,
			Removable:      bool(device.Removable),
			ReadOnly:       bool(device.ReadOnly),
			Transport:      strings.ToLower(strings.TrimSpace(device.Transport)),
			ParentDevice:   parentDevice(device, parentPath),
			ObservedAt:     observedAt,
		})
	}
	for _, child := range device.Children {
		// A partition inherits its parent's transport when lsblk reports it
		// only on the whole disk, which is the common case for USB.
		if child.Transport == "" {
			child.Transport = device.Transport
		}
		if !bool(child.Removable) && bool(device.Removable) {
			child.Removable = device.Removable
		}
		result = append(result, flatten(child, path, observedAt)...)
	}
	return result
}

func parentDevice(device lsblkDevice, parentPath string) string {
	if device.ParentName != "" {
		if strings.HasPrefix(device.ParentName, "/dev/") {
			return device.ParentName
		}
		return "/dev/" + device.ParentName
	}
	return parentPath
}
