// +build windows,integration

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"os"
	"os/exec"
)

func main() {
	//var pidFlag = flag.Int("pid", -1, "PID of the process to kill")
	//flag.Parse()
	//if *pidFlag == -1 {
	//	flag.Usage()
	//	os.Exit(1)
	//}
	//p, _ := os.FindProcess(*pidFlag)
	//p.Kill()

	kill := exec.Command("TASKKILL", "/T", "/F", "/IM amazon-ssm-agent.exe")
	kill.Stderr = os.Stderr
	kill.Stdout = os.Stdout
	kill.Run()
}