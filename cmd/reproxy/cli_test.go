package main

import "testing"

// Which program the arguments are for has to be decided before either one
// parses them, and getting it wrong means `reproxy --json findings ls`
// opens a window (or the app's flags land in the command line). These are
// the cases that decide it.
func TestIsCommandLine(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{args: nil, want: false},                                    // the app
		{args: []string{"-headless"}, want: false},                  // the app, with flags
		{args: []string{"-port", "8080"}, want: false},              //
		{args: []string{"--help"}, want: false},                     // the app's own usage
		{args: []string{"-version"}, want: false},                   //
		{args: []string{"plugins", "ls"}, want: true},               // the command line
		{args: []string{"shell"}, want: true},                       //
		{args: []string{"help"}, want: true},                        //
		{args: []string{"--json", "findings", "ls"}, want: true},    // a global before the namespace
		{args: []string{"--instance", "ab12", "flows"}, want: true}, // one that takes a value
		{args: []string{"--instance=ab12", "flows"}, want: true},    //
		{args: []string{"-i", "-headless", "plugins"}, want: true},  // -headless is the instance's name here
		{args: []string{"--json"}, want: false},                     // a global and nothing to run
		{args: []string{"-port", "8080", "plugins"}, want: false},   // an app flag wins: this isn't a command
	} {
		if got := isCommandLine(tc.args); got != tc.want {
			t.Errorf("isCommandLine(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
