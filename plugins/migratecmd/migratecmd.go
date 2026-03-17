// Package migratecmd adds a new "migrate" command support to a PocketBase instance.
//
// It also comes with automigrations support and templates generation
// (both for JS and GO migration files).
//
// Example usage:
//
//	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
//		TemplateLang: migratecmd.TemplateLangJS, // default to migratecmd.TemplateLangGo
//		Automigrate:  true,
//		Dir:          "/custom/migrations/dir", // optional template migrations path; default to "pb_migrations" (for JS) and "migrations" (for Go)
//	})
//
//	Note: To allow running JS migrations you'll need to enable first
//	[jsvm.MustRegister()].
package migratecmd

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/inflector"
	"github.com/pocketbase/pocketbase/tools/osutils"
	"github.com/spf13/cobra"
)

// Config defines the config options of the migratecmd plugin.
type Config struct {
	// Dir specifies the directory with the user defined migrations.
	//
	// If not set it fallbacks to a relative "pb_data/../pb_migrations" (for js)
	// or "pb_data/../migrations" (for go) directory.
	Dir string

	// Automigrate specifies whether to enable automigrations.
	Automigrate bool

	// TemplateLang specifies the template language to use when
	// generating migrations - js or go (default).
	TemplateLang string
}

// MustRegister registers the migratecmd plugin to the provided app instance
// and panic if it fails.
//
// Example usage:
//
//	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{})
func MustRegister(app core.App, rootCmd *cobra.Command, config Config) {
	if err := Register(app, rootCmd, config); err != nil {
		panic(err)
	}
}

// Register registers the migratecmd plugin to the provided app instance.
func Register(app core.App, rootCmd *cobra.Command, config Config) error {
	p := &plugin{app: app, config: config}

	if p.config.TemplateLang == "" {
		p.config.TemplateLang = TemplateLangGo
	}

	if p.config.Dir == "" {
		if p.config.TemplateLang == TemplateLangJS {
			p.config.Dir = filepath.Join(p.app.DataDir(), "../pb_migrations")
		} else {
			p.config.Dir = filepath.Join(p.app.DataDir(), "../migrations")
		}
	}

	// attach the migrate command
	if rootCmd != nil {
		rootCmd.AddCommand(p.createCommand())
	}

	// watch for collection changes
	if p.config.Automigrate {
		p.app.OnCollectionCreateRequest().BindFunc(p.automigrateOnCollectionChange)
		p.app.OnCollectionUpdateRequest().BindFunc(p.automigrateOnCollectionChange)
		p.app.OnCollectionDeleteRequest().BindFunc(p.automigrateOnCollectionChange)
	}

	return nil
}

type plugin struct {
	app    core.App
	config Config
}

func (p *plugin) createCommand() *cobra.Command {
	const cmdDesc = `Supported arguments are:
- up            - runs all available migrations
- down [number] - reverts the last [number] applied migrations
- create name   - creates new blank migration template file
- collections   - creates new migration file with snapshot of the local collections configuration
- history-sync  - ensures that the _migrations history table doesn't have references to deleted migration files
- status        - shows the current status (applied/pending) of all migrations
- fresh         - reverts all applied migrations and re-runs them from scratch
`

	command := &cobra.Command{
		Use:          "migrate",
		Short:        "Executes app DB migration scripts",
		Long:         cmdDesc,
		ValidArgs:    []string{"up", "down", "create", "collections", "history-sync", "status", "fresh"},
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			cmd := ""
			if len(args) > 0 {
				cmd = args[0]
			}

			switch cmd {
			case "create":
				if _, err := p.migrateCreateHandler("", args[1:], true); err != nil {
					return err
				}
			case "collections":
				if _, err := p.migrateCollectionsHandler(args[1:], true); err != nil {
					return err
				}
			case "status":
				if err := p.migrateStatusHandler(); err != nil {
					return err
				}
			case "fresh":
				if err := p.migrateFreshHandler(); err != nil {
					return err
				}
			default:
				// note: system migrations are always applied as part of the bootstrap process
				var list = core.MigrationsList{}
				list.Copy(core.SystemMigrations)
				list.Copy(core.AppMigrations)

				runner := core.NewMigrationsRunner(p.app, list)

				if err := runner.Run(args...); err != nil {
					return err
				}
			}

			return nil
		},
	}

	return command
}

func (p *plugin) migrateCreateHandler(template string, args []string, interactive bool) (string, error) {
	if len(args) < 1 {
		return "", errors.New("missing migration file name")
	}

	name := args[0]
	dir := p.config.Dir

	filename := fmt.Sprintf("%d_%s.%s", time.Now().Unix(), inflector.Snakecase(name), p.config.TemplateLang)

	resultFilePath := path.Join(dir, filename)

	if interactive {
		confirm := osutils.YesNoPrompt(fmt.Sprintf("Do you really want to create migration %q?", resultFilePath), false)
		if !confirm {
			fmt.Println("The command has been cancelled")
			return "", nil
		}
	}

	// get default create template
	if template == "" {
		var templateErr error
		if p.config.TemplateLang == TemplateLangJS {
			template, templateErr = p.jsBlankTemplate()
		} else {
			template, templateErr = p.goBlankTemplate()
		}
		if templateErr != nil {
			return "", fmt.Errorf("failed to resolve create template: %v", templateErr)
		}
	}

	// ensure that the migrations dir exist
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return "", err
	}

	// save the migration file
	if err := os.WriteFile(resultFilePath, []byte(template), 0644); err != nil {
		return "", fmt.Errorf("failed to save migration file %q: %v", resultFilePath, err)
	}

	if interactive {
		fmt.Printf("Successfully created file %q\n", resultFilePath)
	}

	return filename, nil
}

func (p *plugin) migrateCollectionsHandler(args []string, interactive bool) (string, error) {
	createArgs := []string{"collections_snapshot"}
	createArgs = append(createArgs, args...)

	collections := []*core.Collection{}
	if err := p.app.CollectionQuery().OrderBy("created ASC").All(&collections); err != nil {
		return "", fmt.Errorf("failed to fetch migrations list: %v", err)
	}

	var template string
	var templateErr error
	if p.config.TemplateLang == TemplateLangJS {
		template, templateErr = p.jsSnapshotTemplate(collections)
	} else {
		template, templateErr = p.goSnapshotTemplate(collections)
	}
	if templateErr != nil {
		return "", fmt.Errorf("failed to resolve template: %v", templateErr)
	}

	return p.migrateCreateHandler(template, createArgs, interactive)
}

// migrateStatusHandler prints the current status of all migrations
// (applied or pending) to stdout.
func (p *plugin) migrateStatusHandler() error {
	var list = core.MigrationsList{}
	list.Copy(core.SystemMigrations)
	list.Copy(core.AppMigrations)

	runner := core.NewMigrationsRunner(p.app, list)

	statuses, err := runner.Status()
	if err != nil {
		return err
	}

	if len(statuses) == 0 {
		color.Yellow("No migrations found.")
		return nil
	}

	// column widths
	const statusCol = 9 // "Applied" or "Pending"
	fmt.Printf("%-*s  %s\n", statusCol, "Status", "Migration")
	fmt.Printf("%s  %s\n", strings.Repeat("-", statusCol), strings.Repeat("-", 9))

	for _, s := range statuses {
		if s.Applied {
			color.Green("%-*s  %s", statusCol, "Applied", s.File)
		} else {
			color.Yellow("%-*s  %s", statusCol, "Pending", s.File)
		}
	}

	return nil
}

// migrateFreshHandler reverts all applied migrations and re-applies them.
func (p *plugin) migrateFreshHandler() error {
	var list = core.MigrationsList{}
	list.Copy(core.SystemMigrations)
	list.Copy(core.AppMigrations)

	runner := core.NewMigrationsRunner(p.app, list)

	confirm := osutils.YesNoPrompt(
		"Do you really want to revert all applied migrations and re-run them from scratch?",
		false,
	)
	if !confirm {
		fmt.Println("The command has been cancelled")
		return nil
	}

	// revert all applied migrations
	reverted, err := runner.Down(len(list.Items()))
	if err != nil {
		return err
	}
	for _, file := range reverted {
		color.Yellow("Reverted %s", file)
	}

	// re-apply all migrations
	applied, err := runner.Up()
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		color.Green("No migrations to apply.")
	} else {
		for _, file := range applied {
			color.Green("Applied %s", file)
		}
	}

	return nil
}
