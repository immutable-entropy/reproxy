package app

import (
	"context"
	"fmt"

	"reproxy/internal/cli"
)

// The instances namespace: the reproxy processes sharing this data
// directory, which is how --instance is answered.

// ---- instances -------------------------------------------------------------

func (a *App) instanceCommands() []cli.Command {
	return []cli.Command{{
		Namespace: "instances",
		Action:    "ls",
		Summary:   "list the reproxy processes sharing this data directory",
		Description: "Each one has its own open engagement, traffic and plugins. " +
			"Any command can be run in a particular one with --instance <id>; " +
			"without it, commands run in the process holding the hub role.",
		Run: a.cmdInstancesList,
	}}
}

func (a *App) cmdInstancesList(ctx context.Context, _ *cli.Input) (cli.Result, error) {
	node := a.Node()
	if node == nil {
		return cli.Result{}, fmt.Errorf("this process has not joined the reproxy group")
	}
	infos := node.Router().Instances(ctx)
	_, hubID, _ := node.Hub()

	rows := make([][]string, 0, len(infos))
	for _, in := range infos {
		role := ""
		if in.ID == hubID {
			role = "hub"
		}
		proxy := in.ProxyAddr
		if in.ProxyErr != "" {
			proxy = "not listening"
		}
		rows = append(rows, []string{in.ID, fmt.Sprint(in.PID), role, proxy, since(in.Started)})
	}
	return cli.Data(cli.Table([]string{"ID", "PID", "ROLE", "PROXY", "UP"}, rows), infos)
}
