// Copyright 2016-2022, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	survey "github.com/AlecAivazis/survey/v2"
	surveycore "github.com/AlecAivazis/survey/v2/core"
	"github.com/opentracing/opentracing-go"
	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	"github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/pkg/v3/util/yamlutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type newPolicyArgs struct {
	dir               string
	force             bool
	generateOnly      bool
	interactive       bool
	offline           bool
	templateNameOrURL string
	yes               bool
}

func newPolicyNewCmd() *cobra.Command {
	args := newPolicyArgs{
		interactive: cmdutil.Interactive(),
	}

	cmd := &cobra.Command{
		Use:        "new [template|url]",
		SuggestFor: []string{"init", "create"},
		Short:      "Create a new Pulumi Policy Pack",
		Long: "Create a new Pulumi Policy Pack from a template.\n" +
			"\n" +
			"To create a Policy Pack from a specific template, pass the template name (such as `aws-typescript`\n" +
			"or `azure-python`).  If no template name is provided, a list of suggested templates will be presented\n" +
			"which can be selected interactively.\n" +
			"\n" +
			"Once you're done authoring the Policy Pack, you will need to publish the pack to your organization.\n" +
			"Only organization administrators can publish a Policy Pack.",
		Args: cmdutil.MaximumNArgs(1),
		Run: cmdutil.RunFunc(func(cmd *cobra.Command, cliArgs []string) error {
			ctx := commandContext()
			if len(cliArgs) > 0 {
				args.templateNameOrURL = cliArgs[0]
			}
			return runNewPolicyPack(ctx, args)
		}),
	}

	cmd.PersistentFlags().StringVar(
		&args.dir, "dir", "",
		"The location to place the generated Policy Pack; if not specified, the current directory is used")
	cmd.PersistentFlags().BoolVarP(
		&args.force, "force", "f", false,
		"Forces content to be generated even if it would change existing files")
	cmd.PersistentFlags().BoolVarP(
		&args.generateOnly, "generate-only", "g", false,
		"Generate the Policy Pack only; do not install dependencies")
	cmd.PersistentFlags().BoolVarP(
		&args.offline, "offline", "o", false,
		"Use locally cached templates without making any network requests")

	return cmd
}

func runNewPolicyPack(ctx context.Context, args newPolicyArgs) error {
	if !args.interactive && !args.yes {
		return errors.New("--yes must be passed in to proceed when running in non-interactive mode")
	}

	// Prepare options.
	opts := display.Options{
		Color:         cmdutil.GetGlobalColorization(),
		IsInteractive: args.interactive,
	}

	// Get the current working directory.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting the working directory: %w", err)
	}

	// If dir was specified, ensure it exists and use it as the
	// current working directory.
	if args.dir != "" {
		cwd, err = useSpecifiedDir(args.dir)
		if err != nil {
			return err
		}
	}

	// Return an error if the directory isn't empty.
	if !args.force {
		if err = errorIfNotEmptyDirectory(cwd); err != nil {
			return err
		}
	}

	// Retrieve the templates-policy repo.
	repo, err := workspace.RetrieveTemplates(args.templateNameOrURL, args.offline, workspace.TemplateKindPolicyPack)
	if err != nil {
		return err
	}
	defer func() {
		contract.IgnoreError(repo.Delete())
	}()

	// List the templates from the repo.
	templates, err := repo.PolicyTemplates()
	if err != nil {
		return err
	}

	var template workspace.PolicyPackTemplate
	if len(templates) == 0 {
		return errors.New("no templates")
	} else if len(templates) == 1 {
		template = templates[0]
	} else {
		if template, err = choosePolicyPackTemplate(templates, opts); err != nil {
			return err
		}
	}

	// Do a dry run, if we're not forcing files to be overwritten.
	if !args.force {
		if err = workspace.CopyTemplateFilesDryRun(template.Dir, cwd, ""); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("template '%s' not found: %w", args.templateNameOrURL, err)
			}
			return err
		}
	}

	// Actually copy the files.
	if err = workspace.CopyTemplateFiles(template.Dir, cwd, args.force, "", ""); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("template '%s' not found: %w", args.templateNameOrURL, err)
		}
		return err
	}

	fmt.Println("Created Policy Pack!")

	proj, projPath, root, err := readPolicyProject()
	if err != nil {
		return err
	}

	if filepath.Ext(projPath) == ".yaml" {
		filedata, err := os.ReadFile(projPath)
		if err != nil {
			return err
		}
		var workspaceDocument yaml.Node
		err = yaml.Unmarshal(filedata, &workspaceDocument)
		if err != nil {
			return err
		}
		if proj.Runtime.Name() == "python" {
			// If the template does give virtualenv use it, else default to "venv"
			if len(proj.Runtime.Options()) == 0 {
				proj.Runtime.SetOption("virtualenv", "venv")
				err = yamlutil.Insert(&workspaceDocument, "runtime", strings.TrimSpace(`
name: python
options:
virtualenv: venv
`))
				if err != nil {
					return err
				}
			}
		}

		contract.Assert(len(workspaceDocument.Content) == 1)
		projFile, err := yaml.Marshal(workspaceDocument.Content[0])
		if err != nil {
			return err
		}

		err = os.WriteFile(projPath, projFile, 0600)
		if err != nil {
			return err
		}
	}

	// Install dependencies.
	if !args.generateOnly {
		span := opentracing.SpanFromContext(ctx)
		// Bit of a hack here. Creating a plugin context requires a "program project", but we've only got a
		// policy project. Ideally we should be able to make a plugin context without any related project. But
		// fow now this works.
		projinfo := &engine.Projinfo{Proj: &workspace.Project{
			Main:    proj.Main,
			Runtime: proj.Runtime}, Root: root}
		_, _, pluginCtx, err := engine.ProjectInfoContext(
			projinfo,
			nil,
			cmdutil.Diag(),
			cmdutil.Diag(),
			false,
			span,
		)
		if err != nil {
			return err
		}

		defer pluginCtx.Close()

		if err := installPolicyPackDependencies(pluginCtx, proj, projPath, root); err != nil {
			return err
		}
	}

	fmt.Println(
		opts.Color.Colorize(
			colors.BrightGreen+colors.Bold+"Your new Policy Pack is ready to go!"+colors.Reset) +
			" " + cmdutil.EmojiOr("✨", ""))
	fmt.Println()

	printPolicyPackNextSteps(proj, root, args.generateOnly, opts)

	return nil
}

func installPolicyPackDependencies(ctx *plugin.Context,
	proj *workspace.PolicyPackProject, projPath, root string) error {
	// First make sure the language plugin is present.  We need this to load the required resource plugins.
	// TODO: we need to think about how best to version this.  For now, it always picks the latest.
	lang, err := ctx.Host.LanguageRuntime(proj.Runtime.Name())
	if err != nil {
		return fmt.Errorf("failed to load language plugin %s: %w", proj.Runtime.Name(), err)
	}

	if err = lang.InstallDependencies(root); err != nil {
		return fmt.Errorf("installing dependencies failed; rerun manually to try again, "+
			"then run `pulumi up` to perform an initial deployment: %w", err)
	}

	return nil
}

func printPolicyPackNextSteps(proj *workspace.PolicyPackProject, root string, generateOnly bool, opts display.Options) {
	var commands []string
	if generateOnly {
		// We didn't install dependencies, so instruct the user to do so.
		if strings.EqualFold(proj.Runtime.Name(), "nodejs") {
			commands = append(commands, "npm install")
		} else if strings.EqualFold(proj.Runtime.Name(), "python") {
			commands = append(commands, pythonCommands()...)
		}
	}

	if len(commands) == 1 {
		installMsg := fmt.Sprintf("To install dependencies for the Policy Pack, run `%s`", commands[0])
		installMsg = colors.Highlight(installMsg, commands[0], colors.BrightBlue+colors.Bold)
		fmt.Println(opts.Color.Colorize(installMsg))
		fmt.Println()
	}

	if len(commands) > 1 {
		fmt.Println("To install dependencies for the Policy Pack, run the following commands:")
		fmt.Println()
		for i, cmd := range commands {
			cmdColors := colors.BrightBlue + colors.Bold + cmd + colors.Reset
			fmt.Printf("   %d. %s\n", i+1, opts.Color.Colorize(cmdColors))
		}
		fmt.Println()
	}

	usageCommandPreambles :=
		[]string{"run the Policy Pack against a Pulumi program, in the directory of the Pulumi program run"}
	usageCommands := []string{fmt.Sprintf("pulumi up --policy-pack %s", root)}

	if strings.EqualFold(proj.Runtime.Name(), "nodejs") || strings.EqualFold(proj.Runtime.Name(), "python") {
		usageCommandPreambles = append(usageCommandPreambles, "publish the Policy Pack, run")
		usageCommands = append(usageCommands, "pulumi policy publish [org-name]")
	}

	contract.Assert(len(usageCommandPreambles) == len(usageCommands))

	if len(usageCommands) == 1 {
		usageMsg := fmt.Sprintf("Once you're done editing your Policy Pack, to %s `%s`", usageCommandPreambles[0],
			usageCommands[0])
		usageMsg = colors.Highlight(usageMsg, usageCommands[0], colors.BrightBlue+colors.Bold)
		fmt.Println(opts.Color.Colorize(usageMsg))
		fmt.Println()
	} else {
		fmt.Println("Once you're done editing your Policy Pack:")
		fmt.Println()
		for i, cmd := range usageCommands {
			cmdColors := colors.BrightBlue + colors.Bold + cmd + colors.Reset
			fmt.Printf("   * To %s `%s`\n", usageCommandPreambles[i], opts.Color.Colorize(cmdColors))
		}
		fmt.Println()
	}
}

// choosePolicyPackTemplate will prompt the user to choose amongst the available templates.
func choosePolicyPackTemplate(templates []workspace.PolicyPackTemplate,
	opts display.Options) (workspace.PolicyPackTemplate, error) {

	const chooseTemplateErr = "no template selected; please use `pulumi policy new` to choose one"
	if !opts.IsInteractive {
		return workspace.PolicyPackTemplate{}, errors.New(chooseTemplateErr)
	}

	// Customize the prompt a little bit (and disable color since it doesn't match our scheme).
	surveycore.DisableColor = true
	message := "\rPlease choose a template:"
	message = opts.Color.Colorize(colors.SpecPrompt + message + colors.Reset)

	options, optionToTemplateMap := policyTemplatesToOptionArrayAndMap(templates)

	var option string
	if err := survey.AskOne(&survey.Select{
		Message:  message,
		Options:  options,
		PageSize: optimalPageSize(optimalPageSizeOpts{nopts: len(options)}),
	}, &option, surveyIcons(opts.Color)); err != nil {
		return workspace.PolicyPackTemplate{}, errors.New(chooseTemplateErr)
	}
	return optionToTemplateMap[option], nil
}

// policyTemplatesToOptionArrayAndMap returns an array of option strings and a map of option strings to policy
// templates. Each option string is made up of the template name and description with some padding in between.
func policyTemplatesToOptionArrayAndMap(
	templates []workspace.PolicyPackTemplate) ([]string, map[string]workspace.PolicyPackTemplate) {

	// Find the longest name length. Used to add padding between the name and description.
	maxNameLength := 0
	for _, template := range templates {
		if len(template.Name) > maxNameLength {
			maxNameLength = len(template.Name)
		}
	}

	// Build the array and map.
	var options []string
	nameToTemplateMap := make(map[string]workspace.PolicyPackTemplate)
	for _, template := range templates {
		// Create the option string that combines the name, padding, and description.
		option := fmt.Sprintf(fmt.Sprintf("%%%ds    %%s", -maxNameLength), template.Name, template.Description)

		// Add it to the array and map.
		options = append(options, option)
		nameToTemplateMap[option] = template
	}
	sort.Strings(options)
	return options, nameToTemplateMap
}
