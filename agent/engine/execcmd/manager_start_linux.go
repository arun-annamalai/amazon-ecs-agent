package execcmd

import apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"

const (
	execAgentCmdUser = "0"
)

func getexecAgentCmdBinDir(ma *apicontainer.ManagedAgent) string {
	return ContainerDepsDirPrefix + ma.ID
}
