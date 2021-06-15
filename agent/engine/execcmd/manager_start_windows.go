package execcmd

import apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"

const (
	execAgentCmdUser   = "NT AUTHORITY\\SYSTEM"
	execAgentCmdBinDir = "C:\\Program Files\\Amazon\\SSM"
)

func getexecAgentCmdBinDir(ma *apicontainer.ManagedAgent) string {
	return execAgentCmdBinDir
}
