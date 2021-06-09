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
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/agent/api/container/status"
	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/cihub/seelog"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/pborman/uuid"
)

const (
	// filePerm is the permission for the exec agent config file.
	filePerm            = 0644
	defaultSessionLimit = 2
)

var (
	namelessContainerPrefix = "nameless-container-"

	ecsAgentExecDepsDir = config.AmazonECSProgramFiles + "\\managed-agents\\execute-command"

	// ecsAgentDepsBinDir is the directory where ECS Agent will read versions of SSM agent
	ecsAgentDepsBinDir   = ecsAgentExecDepsDir + "\\bin"
	ecsAgentDepsCertsDir = ecsAgentExecDepsDir + "\\certs"

	HostDepsDirPrefix      = config.AmazonECSProgramFiles + "\\managed-agents\\execute-command\\container-dependencies-"
	ContainerDepsDirPrefix = config.AmazonECSProgramFiles + "\\managed-agents\\execute-command-"

	SSMAgentBinName       = "amazon-ssm-agent.exe"
	SSMAgentWorkerBinName = "ssm-agent-worker.exe"
	SessionWorkerBinName  = "ssm-session-worker.exe"

	HostLogDir      = config.AmazonECSProgramData + "\\exec"
	ContainerLogDir = config.AmazonProgramData + "\\SSM"

	// since ecs agent windows is not running in a container, the agent and host log dirs are the same
	ECSAgentExecLogDir = config.AmazonECSProgramData + "\\exec"

	HostCertFile            = ecsAgentDepsCertsDir + "\\tls-ca-bundle.pem"
	ContainerCertFileSuffix = "certs\\amazon-ssm-agent.crt"

	containerConfigFileName   = "amazon-ssm-agent.json"
	ContainerConfigDirName    = "config"
	ContainerConfigFileSuffix = "configuration\\" + containerConfigFileName

	// ECSAgentExecConfigDir is the directory where ECS Agent will write the ExecAgent config files to
	ECSAgentExecConfigDir = ecsAgentExecDepsDir + "\\" + ContainerConfigDirName
	// HostExecConfigDir is the dir where ExecAgents Config files will live
	HostExecConfigDir          = hostExecDepsDir + "\\" + ContainerConfigDirName
	ExecAgentLogConfigFileName = "seelog.xml"
	ContainerLogConfigFile     = "configuration\\" + ExecAgentLogConfigFileName
)

var (
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

	// TODO: [ecs-exec] seelog config needs to be implemented following a similar approach to ss, config
	execAgentConfigFileNameTemplate    = `amazon-ssm-agent-%s.json`
	logConfigFileNameTemplate          = `seelog-%s.xml`
	errExecCommandManagedAgentNotFound = fmt.Errorf("managed agent not found (%s)", ExecuteCommandAgentName)
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

	// TODO: remove this
	seelog.Error("windows init container has run")

	ma, ok := container.GetManagedAgentByName(ExecuteCommandAgentName)
	if !ok {
		return errExecCommandManagedAgentNotFound
	}
	_, rErr = GetExecAgentConfigFileName(getSessionWorkersLimit(ma))
	if rErr != nil {
		rErr = fmt.Errorf("could not generate ExecAgent Config File: %v", rErr)
		return rErr
	}
	_, rErr = GetExecAgentLogConfigFile()
	if rErr != nil {
		rErr = fmt.Errorf("could not generate ExecAgent LogConfig file: %v", rErr)
		return rErr
	}

	uuid := newUUID()
	containerDepsFolder := ContainerDepsDirPrefix + uuid

	latestBinVersionDir, rErr := m.getLatestVersionedHostBinDir()
	if rErr != nil {
		return rErr
	}

	if !certsExist() {
		rErr = fmt.Errorf("could not find certs")
		return rErr
	}

	// Note in Windows, file bind mounts are not supported so the binaries and configs are copied
	// to a dir and entire dir is mounted
	rErr = createTaskDepsDir(taskId)

	if rErr != nil {
		rErr = fmt.Errorf("could not create task dependency directory")
		return rErr
	}
	// Need to copy files over to container deps Folder
	HostDepsDir := HostDepsDirPrefix + taskId
	// Copy ssm binary files
	copyDirFiles(latestBinVersionDir, HostDepsDir)

	// Copy exec agent config files
	copyDirFiles(HostExecConfigDir, HostDepsDir)

	// Append TLS cert mount
	// TODO: confirm that certs are not needed for Windows

	// TODO: The SSM Agent has to run in this dir: C:\Program Files\Amazon\SSM
	containerDepsFolder = "C:\\Program Files\\Amazon\\SSM"
	// bind mount shared dependency folder containing bin and configs
	hostConfig.Binds = append(hostConfig.Binds, getReadOnlyBindMountMapping(
		HostDepsDirPrefix+taskId,
		containerDepsFolder))

	// Add ssm log bind mount
	cn := fileSystemSafeContainerName(container)

	// In windows mounts are not created automatically, so need create
	err := os.MkdirAll(filepath.Join(HostLogDir, taskId, cn), 0755)
	if err != nil {
		return err
	}

	hostConfig.Binds = append(hostConfig.Binds, getBindMountMapping(
		filepath.Join(HostLogDir, taskId, cn),
		ContainerLogDir))

	container.UpdateManagedAgentByName(ExecuteCommandAgentName, apicontainer.ManagedAgentState{
		ID: uuid,
	})

	return nil
}

func (m *manager) getLatestVersionedHostBinDir() (string, error) {
	versions, err := retrieveAgentVersions(ecsAgentDepsBinDir)
	if err != nil {
		return "", err
	}
	sort.Sort(sort.Reverse(byAgentVersion(versions)))

	var latest string
	for _, v := range versions {
		vStr := v.String()
		ecsAgentDepsVersionedBinDir := filepath.Join(ecsAgentDepsBinDir, vStr)
		if !fileExists(filepath.Join(ecsAgentDepsVersionedBinDir, SSMAgentBinName)) {
			continue // try falling back to the previous version
		}
		// TODO: [ecs-exec] This requirement will be removed for SSM agent V2
		if !fileExists(filepath.Join(ecsAgentDepsVersionedBinDir, SSMAgentWorkerBinName)) {
			continue // try falling back to the previous version
		}
		if !fileExists(filepath.Join(ecsAgentDepsVersionedBinDir, SessionWorkerBinName)) {
			continue // try falling back to the previous version
		}
		latest = filepath.Join(m.hostBinDir, vStr)
		break
	}
	if latest == "" {
		return "", fmt.Errorf("no valid versions were found in %s", m.hostBinDir)
	}

	// TODO: remove this
	seelog.Error("windows init container has finished running")
	return latest, nil
}

func getReadOnlyBindMountMapping(hostDir, containerDir string) string {
	return getBindMountMapping(hostDir, containerDir) + ":ro"
}

func getBindMountMapping(hostDir, containerDir string) string {
	return hostDir + ":" + containerDir
}

var newUUID = uuid.New

func fileSystemSafeContainerName(c *apicontainer.Container) string {
	// Trim leading hyphens since they're not valid directory names
	cn := strings.TrimLeft(c.Name, "-")
	if cn == "" {
		// Fallback name in the extreme case that we end up with an empty string after trimming all leading hyphens.
		return namelessContainerPrefix + newUUID()
	}
	return cn
}

func getSessionWorkersLimit(ma apicontainer.ManagedAgent) int {
	// TODO [ecs-exec] : verify that returning the default session limit (2) is ok in case of any errors, misconfiguration
	limit := defaultSessionLimit
	if ma.Properties == nil { // This means ACS didn't send the limit
		return limit
	}
	limitStr, ok := ma.Properties["sessionLimit"]
	if !ok { // This also means ACS didn't send the limit
		return limit
	}
	limit, err := strconv.Atoi(limitStr)
	if err != nil { // This means ACS send a limit that can't be converted to an int
		return limit
	}
	if limit <= 0 {
		limit = defaultSessionLimit
	}
	return limit
}

var GetExecAgentLogConfigFile = getAgentLogConfigFile

func getAgentLogConfigFile() (string, error) {
	hash := getExecAgentConfigHash(execAgentLogConfigTemplate)
	logConfigFileName := fmt.Sprintf(logConfigFileNameTemplate, hash)
	// check if config file exists already
	logConfigFilePath := filepath.Join(ECSAgentExecConfigDir, logConfigFileName)
	//todo: remove this or solve elsewhere
	logConfigFilePath = filepath.Join(ECSAgentExecConfigDir, "seelog.xml")
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

var removeAll = os.RemoveAll

func validConfigExists(configFilePath, expectedHash string) bool {
	config, err := getFileContent(configFilePath)
	if err != nil {
		return false
	}
	hash := getExecAgentConfigHash(string(config))
	return strings.Compare(expectedHash, hash) == 0
}

var getFileContent = readFileContent

func readFileContent(filePath string) ([]byte, error) {
	return ioutil.ReadFile(filePath)
}

var GetExecAgentConfigFileName = getAgentConfigFileName

func getAgentConfigFileName(sessionLimit int) (string, error) {
	config := fmt.Sprintf(execAgentConfigTemplate, sessionLimit)
	hash := getExecAgentConfigHash(config)
	configFileName := fmt.Sprintf(execAgentConfigFileNameTemplate, hash)
	// check if config file exists already
	configFilePath := filepath.Join(ECSAgentExecConfigDir, configFileName)
	//todo: remove this, or find a solution later
	configFilePath = filepath.Join(ECSAgentExecConfigDir, "amazon-ssm-agent.json")
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

func getExecAgentConfigHash(config string) string {
	hash := sha256.New()
	hash.Write([]byte(config))
	return base64.URLEncoding.EncodeToString(hash.Sum(nil))
}

var osStat = os.Stat

func fileExists(path string) bool {
	if fi, err := osStat(path); err == nil {
		return !fi.IsDir()
	}
	return false
}

func isDir(path string) bool {
	if fi, err := osStat(path); err == nil {
		return fi.IsDir()
	}
	return false
}

var createNewExecAgentConfigFile = createNewConfigFile

func createNewConfigFile(config, configFilePath string) error {
	return ioutil.WriteFile(configFilePath, []byte(config), filePerm)
}

func certsExist() bool {
	return fileExists(filepath.Join(ecsAgentDepsCertsDir, "tls-ca-bundle.pem"))
}

func copyDirFiles(srcDir string, destDir string) error {
	files, err := ioUtilReadDir(srcDir)
	if err != nil {
		return err
	}

	for _, f := range files {
		new, err := os.Create(filepath.Join(destDir, f.Name()))
		if err != nil {
			return err
		}

		original, err := os.Open(filepath.Join(srcDir, f.Name()))
		if err != nil {
			return err
		}

		_, err = io.Copy(new, original)
		if err != nil {
			return err
		}
	}

	return nil
}

func createTaskDepsDir(taskId string) error {
	err := os.MkdirAll(HostDepsDirPrefix+taskId, 0755)
	if err != nil {
		return err
	}
	return nil
}
