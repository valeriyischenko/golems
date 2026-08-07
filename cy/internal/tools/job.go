package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// jobChildArg is the argument this binary re-execs itself with to become a
// job's supervisor.
const jobChildArg = "__cy_job_run"

const jobResultFile = "result.json"

// jobResult is a job's fate as a file: written by the supervisor, read by
// whichever Cy asks next, which is not necessarily the one that started it.
type jobResult struct {
	Status     string    `json:"status"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	Pid        int       `json:"pid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// RunJobChildIfRequested turns this process into a job's supervisor when Cy
// re-execs itself as one, and reports whether it did.
//
// A supervisor sits between Cy and the program a job runs, and its only work is
// to write down how that program ended. Cy forks it and waits on it, so today
// it reports nothing Cy could not read from the exit status directly. The point
// is the direction. You cannot wait on a process you did not fork, so work that
// outlives the Cy that started it can only report by leaving a file behind, and
// delivery therefore has to read files. A path taken only after a restart is a
// path that is never right, so the file is what delivery reads from the first
// job onward.
//
// The contract is argv rather than our own environment variables, so that a
// launcher named in configuration can be an ordinary shell script:
//
//	<launcher> <mailbox-directory> <program> [arguments...]
//
// Everything else the program needs — environment, working directory, standard
// streams — is what Cy prepared, inherited and passed straight down.
func RunJobChildIfRequested() bool {
	if len(os.Args) < 2 || os.Args[1] != jobChildArg {
		return false
	}
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "job: expected a mailbox directory and a program to run")
		os.Exit(126)
	}
	hardenSupervisor()
	os.Exit(superviseJob(os.Args[2], os.Args[3], os.Args[4:]))
	return true
}

func superviseJob(mailbox, program string, args []string) int {
	started := time.Now().UTC()
	command := exec.Command(program, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	waitErr := command.Run()

	result := jobResult{
		Status:     jobCompleted,
		ExitCode:   processExitCode(waitErr),
		Error:      processErrorText(waitErr),
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
	}
	if waitErr != nil {
		result.Status = jobFailed
	}
	if command.Process != nil {
		result.Pid = command.Process.Pid
	}
	if err := writeJobResult(mailbox, result); err != nil {
		fmt.Fprintf(os.Stderr, "job: record result: %v\n", err)
	}
	if result.ExitCode != nil {
		return *result.ExitCode
	}
	// The program never ran, or ended in a way with no code of its own. 126 is
	// what a shell says for "found it, could not run it".
	return 126
}

// superviseCommand puts the launcher in front of a command Cy would otherwise
// fork itself. The launcher becomes the process group leader, so cancelling the
// job still cancels everything under it in one signal.
func superviseCommand(launcher []string, mailbox string, command *exec.Cmd) *exec.Cmd {
	args := append(append([]string{}, launcher[1:]...), mailbox, command.Path)
	args = append(args, command.Args[1:]...)
	supervised := exec.Command(launcher[0], args...)
	supervised.Dir = command.Dir
	supervised.Env = command.Env
	return supervised
}

// writeJobResult publishes through a rename, so a reader sees either a whole
// result or none. The reader can be a different process.
func writeJobResult(mailbox string, result jobResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	temporary := filepath.Join(mailbox, jobResultFile+".tmp")
	if err := os.WriteFile(temporary, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(mailbox, jobResultFile))
}

// readJobResult reports what a supervisor left behind, and whether it left
// anything. Nothing is the ordinary case for a job still running, or for one
// killed hard enough that its supervisor never got to write.
func readJobResult(mailbox string) (jobResult, bool) {
	if mailbox == "" {
		return jobResult{}, false
	}
	raw, err := os.ReadFile(filepath.Join(mailbox, jobResultFile))
	if err != nil {
		return jobResult{}, false
	}
	var result jobResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return jobResult{}, false
	}
	return result, true
}
