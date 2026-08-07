package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// jobChildArg is the argument this binary re-execs itself with to become a
// job's supervisor.
const jobChildArg = "__cy_job_run"

const (
	jobResultFile  = "result.json"
	jobOutputFile  = "output"
	jobCommandFile = "command"
)

// jobResult is a job's fate as a file: written by the supervisor, read by
// whichever Cy asks next, which is not necessarily the one that started it.
//
// OutputBytes counts everything the job printed, not what was kept, so a reader
// of the output file beside this one can still say how much went missing.
type jobResult struct {
	Status      string    `json:"status"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	Error       string    `json:"error,omitempty"`
	Pid         int       `json:"pid,omitempty"`
	OutputBytes int64     `json:"output_bytes,omitempty"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
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
	// Passed through to Cy as before and kept a second time, because the copy
	// Cy holds dies with Cy.
	//
	// Whether the two streams are one is the caller's decision, not the
	// supervisor's: Cy hands down either one pipe or two, and the same writer
	// for both is what makes os/exec give the child one pipe rather than two.
	// A shell wants them merged in the order they happened; a configured tool
	// wants its answer on stdout kept clear of its logging on stderr. The file
	// holds both either way, which is the one place the distinction is lost.
	log := &jobBuffer{limit: defaultCommandLogLimit}
	command.Stdout = io.MultiWriter(os.Stdout, log)
	if sameStream(os.Stdout, os.Stderr) {
		command.Stderr = command.Stdout
	} else {
		command.Stderr = io.MultiWriter(os.Stderr, log)
	}
	if err := command.Start(); err != nil {
		// The program never ran. Nobody upstream can see this -- Cy forked the
		// supervisor, and the supervisor started fine -- so it has to be said
		// here or not at all.
		if err := writeJobResult(mailbox, jobResult{
			Status: jobNotStarted, Error: err.Error(),
			StartedAt: started, FinishedAt: time.Now().UTC(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "job: record result: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "job: %v\n", err)
		return 126
	}
	waitErr := command.Wait()

	stored, discarded := log.stats()
	result := jobResult{
		Status:      jobCompleted,
		ExitCode:    processExitCode(waitErr),
		Error:       processErrorText(waitErr),
		OutputBytes: stored + discarded,
		StartedAt:   started,
		FinishedAt:  time.Now().UTC(),
	}
	if waitErr != nil {
		result.Status = jobFailed
	}
	if command.Process != nil {
		result.Pid = command.Process.Pid
	}
	// Output first. The result file is the marker that says the job is over, so
	// publishing it last is what makes "there is a result" also mean "the output
	// beside it is complete".
	tail, _ := log.snapshot(0)
	if err := writeJobFile(mailbox, jobOutputFile, tail); err != nil {
		fmt.Fprintf(os.Stderr, "job: record output: %v\n", err)
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

// sameStream reports whether two of this process's streams are the same open
// file, which for a pipe means the same pipe. Unreadable either way is treated
// as separate: two streams is the general case and the safe guess.
func sameStream(a, b *os.File) bool {
	first, err := a.Stat()
	if err != nil {
		return false
	}
	second, err := b.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(first, second)
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

func writeJobResult(mailbox string, result jobResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return writeJobFile(mailbox, jobResultFile, append(raw, '\n'))
}

// writeJobFile publishes through a rename, so a reader sees either the whole
// file or none of it. The reader can be a different process.
func writeJobFile(mailbox, name string, data []byte) error {
	temporary := filepath.Join(mailbox, name+".tmp")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(mailbox, name))
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

// readJobCommand reports what the job was asked to run, or a stand-in when the
// note is missing, so that a list of jobs is readable either way.
func readJobCommand(mailbox string) string {
	raw, err := os.ReadFile(filepath.Join(mailbox, jobCommandFile))
	if err != nil || len(raw) == 0 {
		return "(command not recorded)"
	}
	return string(raw)
}

// readJobOutput reports the tail the supervisor kept. Only ever present next to
// a result: while the job runs, the output exists solely in the pipe.
func readJobOutput(mailbox string) ([]byte, bool) {
	if mailbox == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(mailbox, jobOutputFile))
	if err != nil {
		return nil, false
	}
	return data, true
}
