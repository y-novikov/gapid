// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package report

import (
	"context"
	"fmt"
	"time"

	"github.com/google/gapid/core/app/crash"
	"github.com/google/gapid/core/app/layout"
	"github.com/google/gapid/core/event/task"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/os/device"
	"github.com/google/gapid/core/os/device/bind"
	"github.com/google/gapid/core/os/device/host"
	"github.com/google/gapid/core/os/file"
	"github.com/google/gapid/core/os/shell"
	"github.com/google/gapid/test/robot/job"
	"github.com/google/gapid/test/robot/job/worker"
	"github.com/google/gapid/test/robot/stash"
)

const (
	reportTimeout = time.Hour
)

type client struct {
	store   *stash.Client
	manager Manager
	tempDir file.Path
}

// Run starts new report client if any hardware is available.
func Run(ctx context.Context, store *stash.Client, manager Manager, tempDir file.Path) error {
	c := &client{store: store, manager: manager, tempDir: tempDir}
	job.OnDeviceAdded(ctx, c.onDeviceAdded)
	host := host.Instance(ctx)
	return manager.Register(ctx, host, host, c.report)
}

func (c *client) onDeviceAdded(ctx context.Context, host *device.Instance, target bind.Device) {
	reportOnTarget := func(ctx context.Context, t *Task) error {
		job.LockDevice(ctx, target)
		defer job.UnlockDevice(ctx, target)
		if target.Status() != bind.Status_Online {
			log.I(ctx, "Trying to report %s on %s not started, device status %s",
				t.Input.Trace, target.Instance().GetSerial(), target.Status().String())
			return nil
		}
		return c.report(ctx, t)
	}
	crash.Go(func() {
		if err := c.manager.Register(ctx, host, target.Instance(), reportOnTarget); err != nil {
			log.E(ctx, "Error running report client: %v", err)
		}
	})
}

func (c *client) report(ctx context.Context, t *Task) error {
	if err := c.manager.Update(ctx, t.Action, job.Running, nil); err != nil {
		return err
	}
	var output *Output
	err := worker.RetryFunction(ctx, 4, time.Millisecond*100, func() (err error) {
		ctx, cancel := task.WithTimeout(ctx, reportTimeout)
		defer cancel()
		output, err = doReport(ctx, t.Action, t.Input, c.store, c.tempDir)
		return
	})
	status := job.Succeeded
	if err != nil {
		status = job.Failed
		log.E(ctx, "Error running report: %v", err)
	} else if output.CallError != "" {
		status = job.Failed
		log.E(ctx, "Error during report: %v", output.CallError)
	}

	return c.manager.Update(ctx, t.Action, status, output)
}

// doReport extracts input files and runs `gapit report` on them, capturing the output. The output object will
// be partially filled in the event of an upload error from store in order to allow examination of the logs.
func doReport(ctx context.Context, action string, in *Input, store *stash.Client, tempDir file.Path) (*Output, error) {
	tracefile := tempDir.Join(action + ".gfxtrace")
	reportfile := tempDir.Join(action + "_report.txt")

	extractedDir := tempDir.Join(action + "_tools")
	extractedLayout := layout.BinLayout(extractedDir)

	gapit, err := extractedLayout.Gapit(ctx)
	if err != nil {
		return nil, err
	}
	gapis, err := extractedLayout.Gapis(ctx)
	if err != nil {
		return nil, err
	}

	defer func() {
		file.Remove(tracefile)
		file.Remove(reportfile)
		file.RemoveAll(extractedDir)
	}()
	if err := store.GetFile(ctx, in.Trace, tracefile); err != nil {
		return nil, err
	}
	if err := store.GetFile(ctx, in.Gapit, gapit); err != nil {
		return nil, err
	}
	if err := store.GetFile(ctx, in.Gapis, gapis); err != nil {
		return nil, err
	}

	if in.GetToolingLayout() != nil {
		gapidAPK, err := extractedLayout.GapidApk(ctx, in.GetToolingLayout().GetGapidAbi())
		if err != nil {
			return nil, err
		}
		if err := store.GetFile(ctx, in.GapidApk, gapidAPK); err != nil {
			return nil, err
		}
	}

	params := []string{
		"report",
		"-gapir-device", in.GetGapirDevice(),
		"-out", reportfile.System(),
		tracefile.System(),
	}
	cmd := shell.Command(gapit.System(), params...)
	output, callErr := cmd.Call(ctx)
	if err := worker.NeedsRetry(output, "Failed to connect to the GAPIS server"); err != nil {
		return nil, err
	}

	outputObj := &Output{}
	if callErr != nil {
		if err := worker.NeedsRetry(callErr.Error()); err != nil {
			return nil, err
		}
		outputObj.CallError = callErr.Error()
	}
	output = fmt.Sprintf("%s\n\n%s", cmd, output)
	log.I(ctx, output)
	logID, err := store.UploadString(ctx, stash.Upload{Name: []string{"report.log"}, Type: []string{"text/plain"}}, output)
	if err != nil {
		return outputObj, err
	}
	outputObj.Log = logID
	reportID, err := store.UploadFile(ctx, reportfile)
	if err != nil {
		return outputObj, err
	}
	outputObj.Report = reportID
	return outputObj, nil
}
