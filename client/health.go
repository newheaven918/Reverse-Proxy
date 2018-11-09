// Copyright 2018 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"net"
	"net/http"
	"time"
)

type HealthCheckMonitor struct {
	checkType      string
	interval       time.Duration
	timeout        time.Duration
	maxFailedTimes int

	// For tcp
	addr string

	// For http
	url string

	failedTimes    uint64
	statusOK       bool
	statusNormalFn func()
	statusFailedFn func()

	ctx    context.Context
	cancel context.CancelFunc
}

func NewHealthCheckMonitor(checkType string, intervalS int, timeoutS int, maxFailedTimes int, addr string, url string,
	statusNormalFn func(), statusFailedFn func()) *HealthCheckMonitor {

	if intervalS <= 0 {
		intervalS = 10
	}
	if timeoutS <= 0 {
		timeoutS = 3
	}
	if maxFailedTimes <= 0 {
		maxFailedTimes = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &HealthCheckMonitor{
		checkType:      checkType,
		interval:       time.Duration(intervalS) * time.Second,
		timeout:        time.Duration(timeoutS) * time.Second,
		maxFailedTimes: maxFailedTimes,
		addr:           addr,
		url:            url,
		statusOK:       false,
		statusNormalFn: statusNormalFn,
		statusFailedFn: statusFailedFn,
		ctx:            ctx,
		cancel:         cancel,
	}
}

func (monitor *HealthCheckMonitor) Start() {
	go monitor.checkWorker()
}

func (monitor *HealthCheckMonitor) Stop() {
	monitor.cancel()
}

func (monitor *HealthCheckMonitor) checkWorker() {
	for {
		ctx, cancel := context.WithDeadline(monitor.ctx, time.Now().Add(monitor.timeout))
		ok := monitor.doCheck(ctx)

		// check if this monitor has been closed
		select {
		case <-ctx.Done():
			cancel()
			return
		default:
			cancel()
		}

		if ok {
			if !monitor.statusOK && monitor.statusNormalFn != nil {
				monitor.statusOK = true
				monitor.statusNormalFn()
			}
		} else {
			monitor.failedTimes++
			if monitor.statusOK && int(monitor.failedTimes) >= monitor.maxFailedTimes && monitor.statusFailedFn != nil {
				monitor.statusOK = false
				monitor.statusFailedFn()
			}
		}

		time.Sleep(monitor.interval)
	}
}

func (monitor *HealthCheckMonitor) doCheck(ctx context.Context) bool {
	switch monitor.checkType {
	case "tcp":
		return monitor.doTcpCheck(ctx)
	case "http":
		return monitor.doHttpCheck(ctx)
	default:
		return false
	}
}

func (monitor *HealthCheckMonitor) doTcpCheck(ctx context.Context) bool {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", monitor.addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (monitor *HealthCheckMonitor) doHttpCheck(ctx context.Context) bool {
	req, err := http.NewRequest("GET", monitor.url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}

	if resp.StatusCode/100 != 2 {
		return false
	}
	return true
}
