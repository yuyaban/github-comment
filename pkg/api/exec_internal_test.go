package api

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/suzuki-shunsuke/github-comment/pkg/config"
	"github.com/suzuki-shunsuke/github-comment/pkg/expr"
)

func TestExecController_getExecConfig(t *testing.T) { //nolint:funlen
	data := []struct {
		title       string
		ctrl        ExecController
		execConfigs []config.ExecConfig
		cmtParams   ExecCommentParams
		exp         config.ExecConfig
		f           bool
		isErr       bool
	}{
		{
			title:       "no exec configs",
			ctrl:        ExecController{},
			execConfigs: []config.ExecConfig{},
			exp:         config.ExecConfig{},
		},
		{
			title: "no exec config matches",
			ctrl: ExecController{
				Expr: expr.Expr{},
			},
			execConfigs: []config.ExecConfig{
				{
					When: "false",
				},
			},
			exp: config.ExecConfig{},
		},
		{
			title: "first matched config is returned",
			ctrl: ExecController{
				Expr: expr.Expr{},
			},
			execConfigs: []config.ExecConfig{
				{
					When:        "true",
					Template:    "foo",
					DontComment: true,
				},
				{
					When:     "true",
					Template: "bar",
				},
			},
			exp: config.ExecConfig{
				When:        "true",
				Template:    "foo",
				DontComment: true,
			},
			f: true,
		},
	}
	for _, d := range data {
		d := d
		t.Run(d.title, func(t *testing.T) {
			execConfig, f, err := d.ctrl.getExecConfig(d.execConfigs, d.cmtParams)
			if d.isErr {
				require.NotNil(t, err)
				return
			}
			require.Nil(t, err)
			require.Equal(t, d.exp, execConfig)
			require.Equal(t, d.f, f)
		})
	}
}
