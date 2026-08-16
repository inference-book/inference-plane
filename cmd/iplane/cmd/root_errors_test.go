package cmd

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// Execute owns error printing. If cobra prints too, every failure arrives
// twice, once as "Error: msg" and once bare, and a multi-line message reads as
// though a second unrelated failure followed the first (#293).
//
// Asserted against the real rootCmd rather than a lookalike, because the
// setting that matters is the one production actually runs with.
func TestCobraDoesNotPrintErrorsItself(t *testing.T) {
	failing := &cobra.Command{
		Use:  "zz-test-failing",
		RunE: func(*cobra.Command, []string) error { return errors.New("boom") },
	}
	rootCmd.AddCommand(failing)
	t.Cleanup(func() { rootCmd.RemoveCommand(failing) })

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs([]string{"zz-test-failing"})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	err := rootCmd.Execute()

	if err == nil {
		t.Fatal("expected the command to fail")
	}
	if got := out.String() + errOut.String(); got != "" {
		t.Errorf("cobra printed the error itself, so Execute's print is a duplicate:\n%s", got)
	}
}

// The other half of the pair. exitWithCode carries an empty message on purpose
// so a scripting-friendly command can set an exit code having already written
// its own stdout, and neither layer should emit a stray "Error:" line for it.
func TestEmptyMessageErrorPrintsNothing(t *testing.T) {
	if msg := exitWithCode(3).Error(); msg != "" {
		t.Errorf("exitWithCode message = %q, want empty so Execute stays silent", msg)
	}
}
