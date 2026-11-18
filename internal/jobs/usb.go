package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"ark/internal/usb"
)

// USBRunner executes background TypeUSB export jobs.
type USBRunner struct {
	dataDir    string
	imageSaver usb.ImageSaver
}

// NewUSBRunner constructs a USBRunner targeting dataDir.
func NewUSBRunner(dataDir string, imageSaver usb.ImageSaver) *USBRunner {
	return &USBRunner{
		dataDir:    dataDir,
		imageSaver: imageSaver,
	}
}

// Run executes the USB export described by job.Metadata / job.Target.
func (r *USBRunner) Run(ctx context.Context, job *Job, progress ProgressFunc) error {
	var cfg usb.ExportConfig
	if job.Metadata != "" {
		if err := json.Unmarshal([]byte(job.Metadata), &cfg); err != nil {
			return fmt.Errorf("invalid usb export metadata: %w", err)
		}
	}
	if cfg.TargetPath == "" {
		cfg.TargetPath = job.Target
	}

	res, err := usb.ExportToUSB(ctx, r.dataDir, cfg, r.imageSaver, func(pct float64, msg string) {
		if progress != nil {
			progress(pct, msg, 0, 0, 0)
		}
	})
	if err != nil {
		return err
	}
	if progress != nil {
		progress(100, fmt.Sprintf("Exported %d files (%d unchanged skipped)", res.FilesCopied, res.FilesSkipped), res.BytesCopied, res.BytesCopied, 0)
	}
	return nil
}
