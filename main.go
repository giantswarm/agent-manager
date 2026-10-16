package main

import (
	"github.com/giantswarm/agent-manager/cmd"
	"github.com/giantswarm/agent-manager/internal/buildinfo"
	"github.com/giantswarm/agent-manager/pkg/project"
)

func main() {
	cmd.SetBuild(buildinfo.Read(project.Version(), project.GitSHA(), project.BuildTimestamp()))
	cmd.Execute()
}
