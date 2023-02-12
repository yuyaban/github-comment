package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/suzuki-shunsuke/github-comment/pkg/config"
	"github.com/suzuki-shunsuke/github-comment/pkg/execute"
	"github.com/suzuki-shunsuke/github-comment/pkg/expr"
	"github.com/suzuki-shunsuke/github-comment/pkg/github"
	"github.com/suzuki-shunsuke/github-comment/pkg/option"
	"github.com/suzuki-shunsuke/github-comment/pkg/template"
	"github.com/suzuki-shunsuke/go-error-with-exit-code/ecerror"
)

type ExecController struct {
	Wd       string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Getenv   func(string) string
	Reader   Reader
	GitHub   GitHub
	Renderer Renderer
	Executor Executor
	Expr     Expr
	Platform Platform
	Config   *config.Config
}

func (ctrl *ExecController) Exec(ctx context.Context, opts *option.ExecOptions) error { //nolint:funlen,cyclop
	if ctrl.Platform != nil {
		if err := ctrl.Platform.ComplementExec(opts); err != nil {
			return fmt.Errorf("complement opts with CI built in environment variables: %w", err)
		}
	}

	if opts.PRNumber == 0 && opts.SHA1 != "" {
		prNum, err := ctrl.GitHub.PRNumberWithSHA(ctx, opts.Org, opts.Repo, opts.SHA1)
		if err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"org":  opts.Org,
				"repo": opts.Repo,
				"sha":  opts.SHA1,
			}).Warn("list associated prs")
		}
		if prNum > 0 {
			opts.PRNumber = prNum
		}
	}

	cfg := ctrl.Config

	if cfg.Base != nil {
		if opts.Org == "" {
			opts.Org = cfg.Base.Org
		}
		if opts.Repo == "" {
			opts.Repo = cfg.Base.Repo
		}
	}

	result, execErr := ctrl.Executor.Run(ctx, &execute.Params{
		Cmd:   opts.Args[0],
		Args:  opts.Args[1:],
		Stdin: ctrl.Stdin,
	})

	if opts.SkipComment {
		if execErr != nil {
			return ecerror.Wrap(execErr, result.ExitCode)
		}
		return nil
	}

	execConfigs, err := ctrl.getExecConfigs(cfg, opts)
	if err != nil {
		return fmt.Errorf("get config: %w", err)
	}

	if err := option.ValidateExec(opts); err != nil {
		return fmt.Errorf("validate command options: %w", err)
	}

	if cfg.Vars == nil {
		cfg.Vars = make(map[string]interface{}, len(opts.Vars))
	}
	for k, v := range opts.Vars {
		cfg.Vars[k] = v
	}
	if cfg.Vars["target"] == nil {
		cfg.Vars["target"] = ""
	}

	ci := ""
	if ctrl.Platform != nil {
		ci = ctrl.Platform.CI()
	}
	joinCommand := strings.Join(opts.Args, " ")
	templates := template.GetTemplates(&template.ParamGetTemplates{
		Templates:      cfg.Templates,
		CI:             ci,
		JoinCommand:    joinCommand,
		CombinedOutput: result.CombinedOutput,
	})
	if err := ctrl.post(ctx, execConfigs, &ExecCommentParams{
		ExitCode:        result.ExitCode,
		Command:         result.Cmd,
		JoinCommand:     joinCommand,
		Stdout:          result.Stdout,
		Stderr:          result.Stderr,
		CombinedOutput:  result.CombinedOutput,
		PRNumber:        opts.PRNumber,
		Org:             opts.Org,
		Repo:            opts.Repo,
		SHA1:            opts.SHA1,
		TemplateKey:     opts.TemplateKey,
		Template:        opts.Template,
		UpdateCondition: opts.UpdateCondition,
		Vars:            cfg.Vars,
	}, templates); err != nil {
		if !opts.Silent {
			fmt.Fprintf(ctrl.Stderr, "github-comment error: %+v\n", err)
		}
	}
	if execErr != nil {
		return ecerror.Wrap(execErr, result.ExitCode)
	}
	return nil
}

type ExecCommentParams struct {
	Stdout         string
	Stderr         string
	CombinedOutput string
	Command        string
	JoinCommand    string
	ExitCode       int
	// PRNumber is the pull request number where the comment is posted
	PRNumber int
	// Org is the GitHub Organization or User name
	Org string
	// Repo is the GitHub Repository name
	Repo string
	// SHA1 is the commit SHA1
	SHA1            string
	TemplateKey     string
	Template        string
	UpdateCondition string
	Vars            map[string]interface{}
}

type Executor interface {
	Run(ctx context.Context, params *execute.Params) (*execute.Result, error)
}

type Expr interface {
	Match(expression string, params interface{}) (bool, error)
	Compile(expression string) (expr.Program, error)
}

func (ctrl *ExecController) getExecConfigs(cfg *config.Config, opts *option.ExecOptions) ([]*config.ExecConfig, error) {
	var execConfigs []*config.ExecConfig
	if opts.Template == "" && opts.TemplateKey != "" {
		a, ok := cfg.Exec[opts.TemplateKey]
		if !ok {
			if opts.TemplateKey != "default" {
				return nil, errors.New("template isn't found: " + opts.TemplateKey)
			}
			execConfigs = []*config.ExecConfig{
				{
					When:            "ExitCode != 0",
					UpdateCondition: `Comment.HasMeta && Comment.Meta.TemplateKey == "default"`,
					Template: `{{template "status" .}} {{template "link" .}}

{{template "join_command" .}}

{{template "hidden_combined_output" .}}`,
				},
			}
		} else {
			execConfigs = a
		}
	}
	return execConfigs, nil
}

// getExecConfig returns matched ExecConfig.
// If no ExecConfig matches, the second returned value is false.
func (ctrl *ExecController) getExecConfig(
	execConfigs []*config.ExecConfig, cmtParams *ExecCommentParams,
) (*config.ExecConfig, bool, error) {
	for _, execConfig := range execConfigs {
		f, err := ctrl.Expr.Match(execConfig.When, cmtParams)
		if err != nil {
			return nil, false, fmt.Errorf("test a condition is matched: %w", err)
		}
		if !f {
			continue
		}
		return execConfig, true, nil
	}
	return nil, false, nil
}

// getComment returns Comment.
// If the second returned value is false, no comment is posted.
func (ctrl *ExecController) getComment(ctx context.Context, execConfigs []*config.ExecConfig, cmtParams *ExecCommentParams, templates map[string]string) (*github.Comment, bool, error) { //nolint:funlen
	tpl := cmtParams.Template
	tplForTooLong := ""
	var embeddedVarNames []string
	var UpdateCondition string
	if tpl == "" {
		execConfig, f, err := ctrl.getExecConfig(execConfigs, cmtParams)
		if err != nil {
			return nil, false, err
		}
		if !f {
			return nil, false, nil
		}
		if execConfig.DontComment {
			return nil, false, nil
		}
		tpl = execConfig.Template
		tplForTooLong = execConfig.TemplateForTooLong
		embeddedVarNames = execConfig.EmbeddedVarNames
		if execConfig.UpdateCondition == "" {
			UpdateCondition = cmtParams.UpdateCondition
		} else {
			UpdateCondition = execConfig.UpdateCondition
		}
	}

	body, err := ctrl.Renderer.Render(tpl, templates, cmtParams)
	if err != nil {
		return nil, false, fmt.Errorf("render a comment template: %w", err)
	}
	bodyForTooLong, err := ctrl.Renderer.Render(tplForTooLong, templates, cmtParams)
	if err != nil {
		return nil, false, fmt.Errorf("render a comment template_for_too_long: %w", err)
	}

	cmtCtrl := CommentController{
		GitHub:   ctrl.GitHub,
		Expr:     ctrl.Expr,
		Getenv:   ctrl.Getenv,
		Platform: ctrl.Platform,
	}

	if !contains(embeddedVarNames, "target") {
		embeddedVarNames = append(embeddedVarNames, "target")
	}

	embeddedMetadata := make(map[string]interface{}, len(embeddedVarNames))
	for _, name := range embeddedVarNames {
		if v, ok := cmtParams.Vars[name]; ok {
			embeddedMetadata[name] = v
		}
	}

	embeddedComment, err := cmtCtrl.getEmbeddedComment(map[string]interface{}{
		"SHA1":        cmtParams.SHA1,
		"TemplateKey": cmtParams.TemplateKey,
		"Vars":        embeddedMetadata,
	})
	if err != nil {
		return nil, false, err
	}

	body += embeddedComment
	bodyForTooLong += embeddedComment

	cmt := github.Comment{
		PRNumber:       cmtParams.PRNumber,
		Org:            cmtParams.Org,
		Repo:           cmtParams.Repo,
		Body:           body,
		BodyForTooLong: bodyForTooLong,
		SHA1:           cmtParams.SHA1,
		Vars:           cmtParams.Vars,
		TemplateKey:    cmtParams.TemplateKey,
	}
	if UpdateCondition != "" && cmtParams.PRNumber != 0 {
		if err := ctrl.setUpdatedCommentID(ctx, &cmt, UpdateCondition); err != nil {
			return nil, false, fmt.Errorf("set updateCommentID: %w", err)
		}
	}

	return &cmt, true, nil
}

func (ctrl *ExecController) setUpdatedCommentID(ctx context.Context, cmt *github.Comment, updateCondition string) error { //nolint:funlen
	custom_updateCondition := fmt.Sprintf("%s && Comment.Meta.Vars.target == \"%s\"", updateCondition, ctrl.Config.Vars["target"])
	prg, err := ctrl.Expr.Compile(custom_updateCondition)
	if err != nil {
		return err //nolint:wrapcheck
	}

	login, err := ctrl.GitHub.GetAuthenticatedUser(ctx)
	if err != nil {
		logrus.WithError(err).Warn("get an authenticated user")
	}

	comments, err := ctrl.GitHub.ListComments(ctx, &github.PullRequest{
		Org:      cmt.Org,
		Repo:     cmt.Repo,
		PRNumber: cmt.PRNumber,
	})
	if err != nil {
		return fmt.Errorf("list issue or pull request comments: %w", err)
	}
	logrus.WithFields(logrus.Fields{
		"org":       cmt.Org,
		"repo":      cmt.Repo,
		"pr_number": cmt.PRNumber,
	}).Debug("get comments")

	for _, comnt := range comments {
		if comnt.IsMinimized {
			// ignore minimized comments
			continue
		}
		if login != "" && comnt.Author.Login != login {
			// ignore other users' comments
			continue
		}

		metadata := map[string]interface{}{}
		hasMeta := extractMetaFromComment(comnt.Body, &metadata)
		paramMap := map[string]interface{}{
			"Comment": map[string]interface{}{
				"Body":    comnt.Body,
				"Meta":    metadata,
				"HasMeta": hasMeta,
			},
			"Commit": map[string]interface{}{
				"Org":      cmt.Org,
				"Repo":     cmt.Repo,
				"PRNumber": cmt.PRNumber,
				"SHA1":     cmt.SHA1,
			},
			"Vars": cmt.Vars,
		}

		logrus.WithFields(logrus.Fields{
			"node_id":   comnt.ID,
			"condition": updateCondition,
			"param":     paramMap,
		}).Debug("judge whether an existing comment is ready for editing")
		f, err := prg.Run(paramMap)
		if err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"node_id": comnt.ID,
			}).Error("judge whether an existing comment is hidden")
			continue
		}
		if !f {
			continue
		}
		cmt.CommentID = comnt.DatabaseID
	}
	return nil
}

func (ctrl *ExecController) post(
	ctx context.Context, execConfigs []*config.ExecConfig, cmtParams *ExecCommentParams,
	templates map[string]string,
) error {
	cmt, f, err := ctrl.getComment(ctx, execConfigs, cmtParams, templates)
	if err != nil {
		return err
	}
	if !f {
		return nil
	}
	logrus.WithFields(logrus.Fields{
		"org":       cmt.Org,
		"repo":      cmt.Repo,
		"pr_number": cmt.PRNumber,
		"sha":       cmt.SHA1,
	}).Debug("comment meta data")

	cmtCtrl := CommentController{
		GitHub: ctrl.GitHub,
		Expr:   ctrl.Expr,
		Getenv: ctrl.Getenv,
	}
	return cmtCtrl.Post(ctx, cmt, map[string]interface{}{
		"Command": map[string]interface{}{
			"ExitCode":       cmtParams.ExitCode,
			"JoinCommand":    cmtParams.JoinCommand,
			"Command":        cmtParams.Command,
			"Stdout":         cmtParams.Stdout,
			"Stderr":         cmtParams.Stderr,
			"CombinedOutput": cmtParams.CombinedOutput,
		},
	})
}
