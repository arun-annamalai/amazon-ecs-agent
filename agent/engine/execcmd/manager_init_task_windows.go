// +build windows

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
	"fmt"
	"os"
	"path/filepath"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/agent/api/container/status"
	"github.com/aws/amazon-ecs-agent/agent/config"
	dockercontainer "github.com/docker/docker/api/types/container"
)

const (
	folderPerm = 0755
)

var (
	ecsAgentExecDepsDir = config.AmazonECSProgramFiles + "\\managed-agents\\execute-command"

	// ecsAgentDepsBinDir is the directory where ECS Agent will read versions of SSM agent
	ecsAgentDepsBinDir    = ecsAgentExecDepsDir + "\\bin"
	ecsAgentDepsConfigDir = ecsAgentExecDepsDir + "\\config"

	containerDepsFolder = "C:\\Program Files\\Amazon\\SSM"

	SSMAgentBinName       = "amazon-ssm-agent.exe"
	SSMAgentWorkerBinName = "ssm-agent-worker.exe"
	SessionWorkerBinName  = "ssm-session-worker.exe"

	HostLogDir      = config.AmazonECSProgramData + "\\exec"
	ContainerLogDir = config.AmazonProgramData + "\\SSM"

	SSMPluginDir = "C:\\Program Files\\Amazon\\SSM\\Plugins"
	// since ecs agent windows is not running in a container, the agent and host log dirs are the same
	ECSAgentExecLogDir = config.AmazonECSProgramData + "\\exec"

	// ECSAgentExecConfigDir is the directory where ECS Agent will write the ExecAgent config files to
	ECSAgentExecConfigDir = ecsAgentExecDepsDir + "\\" + ContainerConfigDirName
	// HostExecConfigDir is the dir where ExecAgents Config files will live
	HostExecConfigDir = hostExecDepsDir + "\\" + ContainerConfigDirName

	execAgentConfigTemplate = `{
"Mgs": {
	"Region": "",
	"Endpoint": "",
	"StopTimeoutMillis": 20000,
	"SessionWorkersLimit": %d
},
"Agent": {
	"Region": "",
	"OrchestrationRootDir": "",
	"ContainerMode": true
}
}`

	execAgentLogConfigTemplate = `<!--amazon-ssm-agent uses seelog logging -->
<!--Seelog has github wiki pages, which contain detailed how-tos references: https://github.com/cihub/seelog/wiki -->
<!--Seelog examples can be found here: https://github.com/cihub/seelog-examples -->
<seelog type="adaptive" mininterval="2000000" maxinterval="100000000" critmsgcount="500" minlevel="info">
    <exceptions>
        <exception filepattern="test*" minlevel="error"/>
    </exceptions>
    <outputs formatid="fmtinfo">
        <console formatid="fmtinfo"/>
        <rollingfile type="size" filename="C:\ProgramData\Amazon\SSM\Logs\amazon-ssm-agent.log" maxsize="30000000" maxrolls="5"/>
        <filter levels="error,critical" formatid="fmterror">
            <rollingfile type="size" filename="C:\ProgramData\Amazon\SSM\Logs\errors.log" maxsize="10000000" maxrolls="5"/>
        </filter>
    </outputs>
    <formats>
        <format id="fmterror" format="%Date %Time %LEVEL [%FuncShort @ %File.%Line] %Msg%n"/>
        <format id="fmtdebug" format="%Date %Time %LEVEL [%FuncShort @ %File.%Line] %Msg%n"/>
        <format id="fmtinfo" format="%Date %Time %LEVEL %Msg%n"/>
    </formats>
</seelog>`
)

func (m *manager) InitializeContainer(taskId string, container *apicontainer.Container, hostConfig *dockercontainer.HostConfig) (rErr error) {
	defer func() {
		if rErr != nil {
			container.UpdateManagedAgentByName(ExecuteCommandAgentName, apicontainer.ManagedAgentState{
				InitFailed: true,
				Status:     apicontainerstatus.ManagedAgentStopped,
				Reason:     rErr.Error(),
			})
		}
	}()

	ma, ok := container.GetManagedAgentByName(ExecuteCommandAgentName)
	if !ok {
		return errExecCommandManagedAgentNotFound
	}

	cn := fileSystemSafeContainerName(container)

	// In windows host mounts are not created automatically, so need to create
	rErr = os.MkdirAll(filepath.Join(HostLogDir, taskId, cn), folderPerm)
	if rErr != nil {
		return rErr
	}

	// In windows host mounts are not created automatically, so need to create
	rErr = os.MkdirAll(filepath.Join(ecsAgentDepsConfigDir, taskId), folderPerm)
	if rErr != nil {
		return rErr
	}

	_, rErr = GetExecAgentConfigFileName(getSessionWorkersLimit(ma), taskId)
	if rErr != nil {
		rErr = fmt.Errorf("could not generate ExecAgent Config File: %v", rErr)
		return rErr
	}
	_, rErr = GetExecAgentLogConfigFile(taskId)
	if rErr != nil {
		rErr = fmt.Errorf("could not generate ExecAgent LogConfig file: %v", rErr)
		return rErr
	}

	uuid := newUUID()

	latestBinVersionDir, rErr := m.getLatestVersionedHostBinDir()
	if rErr != nil {
		return rErr
	}

	// Add ssm binary mount
	hostConfig.Binds = append(hostConfig.Binds, getReadOnlyBindMountMapping(
		latestBinVersionDir,
		containerDepsFolder))

	// Add ssm configuration dir mount
	hostConfig.Binds = append(hostConfig.Binds, getReadOnlyBindMountMapping(
		filepath.Join(ecsAgentDepsConfigDir, taskId),
		filepath.Join(containerDepsFolder, "configuration")))

	// Add ssm log bind mount
	hostConfig.Binds = append(hostConfig.Binds, getBindMountMapping(
		filepath.Join(HostLogDir, taskId, cn),
		ContainerLogDir))

	// add ssm plugin bind mount (needed for execcmd windows)
	hostConfig.Binds = append(hostConfig.Binds, getBindMountMapping(
		SSMPluginDir,
		SSMPluginDir))

	container.UpdateManagedAgentByName(ExecuteCommandAgentName, apicontainer.ManagedAgentState{
		ID: uuid,
	})

	return nil
}

func getAgentLogConfigFile(taskId string) (string, error) {
	hash := getExecAgentConfigHash(execAgentLogConfigTemplate)
	logConfigFileName := "seelog.xml"

	// check if config file exists already
	logConfigFilePath := filepath.Join(ECSAgentExecConfigDir, taskId, logConfigFileName)
	if fileExists(logConfigFilePath) && validConfigExists(logConfigFilePath, hash) {
		return logConfigFileName, nil
	}
	// check if config file is a dir; if true, remove it
	if isDir(logConfigFilePath) {
		if err := removeAll(logConfigFilePath); err != nil {
			return "", err
		}
	}
	// create new seelog config file
	if err := createNewExecAgentConfigFile(execAgentLogConfigTemplate, logConfigFilePath); err != nil {
		return "", err
	}
	return logConfigFileName, nil
}

func getAgentConfigFileName(sessionLimit int, taskId string) (string, error) {
	config := fmt.Sprintf(execAgentConfigTemplate, sessionLimit)
	hash := getExecAgentConfigHash(config)
	configFileName := "amazon-ssm-agent.json"

	// check if config file exists already
	configFilePath := filepath.Join(ECSAgentExecConfigDir, taskId, configFileName)
	if fileExists(configFilePath) && validConfigExists(configFilePath, hash) {
		return configFileName, nil
	}
	// check if config file is a dir; if true, remove it
	if isDir(configFilePath) {
		if err := removeAll(configFilePath); err != nil {
			return "", err
		}
	}
	// config doesn't exist; create a new one
	if err := createNewExecAgentConfigFile(config, configFilePath); err != nil {
		return "", err
	}
	return configFileName, nil
}
