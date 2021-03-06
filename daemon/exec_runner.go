// This file is part of Graylog.
//
// Graylog is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Graylog is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with Graylog.  If not, see <http://www.gnu.org/licenses/>.

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/Graylog2/collector-sidecar/backends"
	"github.com/Graylog2/collector-sidecar/common"
	"github.com/Graylog2/collector-sidecar/context"
)

type ExecRunner struct {
	RunnerCommon
	exec           string
	args           []string
	stderr, stdout string
	isRunning      bool
	restartCount   int
	startTime      time.Time
	cmd            *exec.Cmd
	service        service.Service
	wg             sync.WaitGroup
}

func init() {
	if err := RegisterBackendRunner("exec", NewExecRunner); err != nil {
		log.Fatal(err)
	}
}

func NewExecRunner(backend backends.Backend, context *context.Ctx) Runner {
	r := &ExecRunner{
		RunnerCommon: RunnerCommon{
			name:    backend.Name(),
			context: context,
			backend: backend,
		},
		exec:         backend.ExecPath(),
		args:         backend.ExecArgs(),
		isRunning:    false,
		restartCount: 1,
		stderr:       filepath.Join(context.UserConfig.LogPath, backend.Name()+"_stderr.log"),
		stdout:       filepath.Join(context.UserConfig.LogPath, backend.Name()+"_stdout.log"),
	}

	return r
}

func (r *ExecRunner) Name() string {
	return r.name
}

func (r *ExecRunner) Running() bool {
	return r.isRunning
}

func (r *ExecRunner) SetDaemon(d *DaemonConfig) {
	r.daemon = d
}

func (r *ExecRunner) BindToService(s service.Service) {
	r.service = s
}

func (r *ExecRunner) GetService() service.Service {
	return r.service
}

func (r *ExecRunner) ValidateBeforeStart() error {
	_, err := exec.LookPath(r.exec)
	if err != nil {
		return backends.SetStatusLogErrorf(r.name, "Failed to find collector executable %q: %v", r.exec, err)
	}
	return nil
}

func (r *ExecRunner) Start(s service.Service) error {
	if err := r.ValidateBeforeStart(); err != nil {
		log.Error(err.Error())
		return err
	}

	r.restartCount = 1
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			r.cmd = exec.Command(r.exec, r.args...)
			r.cmd.Dir = r.daemon.Dir
			r.cmd.Env = append(os.Environ(), r.daemon.Env...)
			r.startTime = time.Now()
			r.run()

			// A backend should stay alive longer than 3 seconds
			if time.Since(r.startTime) < 3*time.Second {
				backends.SetStatusLogErrorf(r.name, "Collector exits immediately, this should not happen! Please check your collector configuration!")
			}
			// After 60 seconds we can reset the restart counter
			if time.Since(r.startTime) > 60*time.Second {
				r.restartCount = 0
			}
			if r.restartCount <= 3 && r.isRunning {
				log.Errorf("[%s] Backend crashed, trying to restart %d/3", r.name, r.restartCount)
				time.Sleep(5 * time.Second)
				r.restartCount += 1
				continue
				// giving up
			} else if r.restartCount > 3 {
				backends.SetStatusLogErrorf(r.name, "Collector failed to start after 3 tries!")
			}

			r.isRunning = false
			break
		}
	}()
	return nil
}

func (r *ExecRunner) Stop(s service.Service) error {
	log.Infof("[%s] Stopping", r.name)

	// deactivate supervisor
	r.isRunning = false

	// give the chance to cleanup resources
	if r.cmd.Process != nil {
		r.cmd.Process.Signal(syscall.SIGHUP)
	}
	time.Sleep(2 * time.Second)

	// in doubt kill the process
	if r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}

	// wait for background routine to finish
	r.wg.Wait()

	return nil
}

func (r *ExecRunner) Restart(s service.Service) error {
	r.Stop(s)
	time.Sleep(2 * time.Second)
	r.Start(s)

	return nil
}

func (r *ExecRunner) run() {
	log.Infof("[%s] Starting (%s driver)", r.name, r.backend.Driver())

	if r.stderr != "" {
		err := common.CreatePathToFile(r.stderr)
		if err != nil {
			backends.SetStatusLogErrorf(r.name, "Failed to create path to collector's stderr log: %s", r.stderr)
		}

		f := common.GetRotatedLog(r.stderr, r.context.UserConfig.LogRotationTime, r.context.UserConfig.LogMaxAge)
		defer f.Close()
		r.cmd.Stderr = f
	}
	if r.stdout != "" {
		err := common.CreatePathToFile(r.stdout)
		if err != nil {
			backends.SetStatusLogErrorf(r.name, "Failed to create path to collector's stdout log: %s", r.stdout)
		}

		f := common.GetRotatedLog(r.stderr, r.context.UserConfig.LogRotationTime, r.context.UserConfig.LogMaxAge)
		defer f.Close()
		r.cmd.Stdout = f
	}

	r.isRunning = true
	r.backend.SetStatus(backends.StatusRunning, "Running")
	r.cmd.Run()

	return
}
