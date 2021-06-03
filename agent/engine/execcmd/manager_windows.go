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

package execcmd

import (
	"os"
	"path/filepath"
)

var (
	// EnvProgramFiles Windows environment variable (C:\Program Files)
	EnvProgramFiles       = os.Getenv("ProgramFiles")
	AmazonProgramFiles    = filepath.Join(EnvProgramFiles, "Amazon")
	capabilityDepsRootDir = filepath.Join(AmazonProgramFiles, "managed-agents")

	// EnvProgramData Windows environment variable (C:\ProgramData)
	EnvProgramData    = os.Getenv("ProgramData")
	AmazonProgramData = filepath.Join(EnvProgramData, "Amazon")

	hostExecDepsDir = filepath.Join(AmazonProgramFiles, "ecs\\deps\\execute-command")
	HostBinDir      = filepath.Join(AmazonProgramFiles, "bin")
)
