package watch

import (
	"fmt"
	"regexp"
	"time"

	"github.com/onsi/ginkgo/formatter"
	"github.com/onsi/ginkgo/ginkgo/command"
	"github.com/onsi/ginkgo/ginkgo/internal"
	"github.com/onsi/ginkgo/ginkgo/interrupthandler"
	"github.com/onsi/ginkgo/types"
)

func BuildWatchCommand() command.Command {
	var suiteConfig = types.NewDefaultSuiteConfig()
	var reporterConfig = types.NewDefaultReporterConfig()
	var cliConfig = types.NewDefaultCLIConfig()
	var goFlagsConfig = types.NewDefaultGoFlagsConfig()

	flags, err := types.BuildWatchCommandFlagSet(&suiteConfig, &reporterConfig, &cliConfig, &goFlagsConfig)
	if err != nil {
		panic(err)
	}
	interruptHandler := interrupthandler.NewInterruptHandler()

	return command.Command{
		Name:          "watch",
		Flags:         flags,
		Usage:         "ginkgo watch <FLAGS> <PACKAGES> -- <PASS-THROUGHS>",
		ShortDoc:      "Watch the passed in <PACKAGES> and runs their tests whenever changes occur.",
		Documentation: "Any arguments after -- will be passed to the test.",
		DocLink:       "watching-for-changes",
		Command: func(args []string, additionalArgs []string) {
			var errors []error
			cliConfig, goFlagsConfig, errors = types.VetAndInitializeCLIAndGoConfig(cliConfig, goFlagsConfig)
			command.AbortIfErrors("Ginkgo detected configuration issues:", errors)

			watcher := &SpecWatcher{
				cliConfig:      cliConfig,
				goFlagsConfig:  goFlagsConfig,
				suiteConfig:    suiteConfig,
				reporterConfig: reporterConfig,
				flags:          flags,

				interruptHandler: interruptHandler,
			}

			watcher.WatchSpecs(args, additionalArgs)
		},
	}
}

type SpecWatcher struct {
	suiteConfig    types.SuiteConfig
	reporterConfig types.ReporterConfig
	cliConfig      types.CLIConfig
	goFlagsConfig  types.GoFlagsConfig
	flags          types.GinkgoFlagSet

	interruptHandler *interrupthandler.InterruptHandler
}

func (w *SpecWatcher) WatchSpecs(args []string, additionalArgs []string) {
	suites, _ := internal.FindSuites(args, w.cliConfig, false)

	if len(suites) == 0 {
		command.AbortWith("Found no test suites")
	}

	fmt.Printf("Identified %d test %s.  Locating dependencies to a depth of %d (this may take a while)...\n", len(suites), internal.PluralizedWord("suite", "suites", len(suites)), w.cliConfig.Depth)
	deltaTracker := NewDeltaTracker(w.cliConfig.Depth, regexp.MustCompile(w.cliConfig.WatchRegExp))
	delta, errors := deltaTracker.Delta(suites)

	fmt.Printf("Watching %d %s:\n", len(delta.NewSuites), internal.PluralizedWord("suite", "suites", len(delta.NewSuites)))
	for _, suite := range delta.NewSuites {
		fmt.Println("  " + suite.Description())
	}

	for suite, err := range errors {
		fmt.Printf("Failed to watch %s: %s\n", suite.PackageName, err)
	}

	if len(suites) == 1 {
		w.updateSeed()
		w.compileAndRun(suites[0], additionalArgs)
	}

	ticker := time.NewTicker(time.Second)

	for {
		select {
		case <-ticker.C:
			suites, _ := internal.FindSuites(args, w.cliConfig, false)
			delta, _ := deltaTracker.Delta(suites)
			coloredStream := formatter.ColorableStdOut

			suites = []internal.TestSuite{}

			if len(delta.NewSuites) > 0 {
				fmt.Fprintln(coloredStream, formatter.F("{{green}}Detected %d new %s:{{/}}", len(delta.NewSuites), internal.PluralizedWord("suite", "suites", len(delta.NewSuites))))
				for _, suite := range delta.NewSuites {
					suites = append(suites, suite.Suite)
					fmt.Fprintln(coloredStream, formatter.Fi(1, "%s", suite.Description()))
				}
			}

			modifiedSuites := delta.ModifiedSuites()
			if len(modifiedSuites) > 0 {
				fmt.Fprintln(coloredStream, formatter.F("{{green}}Detected changes in:{{/}}"))
				for _, pkg := range delta.ModifiedPackages {
					fmt.Fprintln(coloredStream, formatter.Fi(1, "%s", pkg))
				}
				fmt.Fprintln(coloredStream, formatter.F("{{green}}Will run %d %s:{{/}}", len(modifiedSuites), internal.PluralizedWord("suite", "suites", len(modifiedSuites))))
				for _, suite := range modifiedSuites {
					suites = append(suites, suite.Suite)
					fmt.Fprintln(coloredStream, formatter.Fi(1, "%s", suite.Description()))
				}
				fmt.Fprintln(coloredStream, "")
			}

			if len(suites) == 0 {
				break
			}

			w.updateSeed()
			w.computeSuccinctMode(len(suites))
			passed := true
			for _, suite := range suites {
				if w.interruptHandler.WasInterrupted() {
					return
				}
				deltaTracker.WillRun(suite)
				passed = w.compileAndRun(suite, additionalArgs) && passed
			}
			color := "{{red}}"
			if passed {
				color = "{{green}}"
			}
			fmt.Fprintln(coloredStream, formatter.F(color+"\nDone.  Resuming watch...{{/}}"))

			messages, err := internal.FinalizeProfilesAndReportsForSuites(suites, w.cliConfig, w.suiteConfig, w.reporterConfig, w.goFlagsConfig)
			command.AbortIfError("could not finalize profiles:", err)
			for _, message := range messages {
				fmt.Println(message)
			}
		case <-w.interruptHandler.InterruptChannel():
			return
		}
	}
}

func (w *SpecWatcher) compileAndRun(suite internal.TestSuite, additionalArgs []string) bool {
	suite = internal.CompileSuite(suite, w.goFlagsConfig)
	if suite.CompilationError != nil {
		fmt.Println(suite.CompilationError.Error())
		return false
	}
	if w.interruptHandler.WasInterrupted() {
		return false
	}
	suite = internal.RunCompiledSuite(suite, w.suiteConfig, w.reporterConfig, w.cliConfig, w.goFlagsConfig, additionalArgs)
	internal.Cleanup(w.goFlagsConfig, suite)
	return suite.Passed
}

func (w *SpecWatcher) computeSuccinctMode(numSuites int) {
	if w.reporterConfig.Verbose {
		w.reporterConfig.Succinct = false
		return
	}

	if w.flags.WasSet("succinct") {
		return
	}

	if numSuites == 1 {
		w.reporterConfig.Succinct = false
	}

	if numSuites > 1 {
		w.reporterConfig.Succinct = true
	}
}

func (w *SpecWatcher) updateSeed() {
	if !w.flags.WasSet("seed") {
		w.suiteConfig.RandomSeed = time.Now().Unix()
	}
}
