package cli

// promptKind names the flavor of a quickstart-tour question, so an injected
// tourPrompt (and the production terminal prompt) can tell a command
// confirmation from the workspace question.
type promptKind int

const (
	// promptCommand is the per-step `Run it? (Y/n)` confirmation preceding each
	// tour command; the empty answer defaults to proceed.
	promptCommand promptKind = iota
	// promptWorkspace is the tour's workspace question: use the current workspace,
	// or create ./iris-quickstart-demo and work there.
	promptWorkspace
)

// promptAnswer is the operator's answer to one tour question.
type promptAnswer int

const (
	// answerProceed runs the step (or accepts the workspace proposal).
	answerProceed promptAnswer = iota
	// answerSkip skips this one step without running it and continues the tour.
	// The workspace question treats it as a decline. The production terminal
	// prompt returns it for "s"/"skip"; it is mainly a seam answer for tests.
	answerSkip
	// answerQuit stops the tour: a clean abort, exit 0 with a resume hint.
	answerQuit
)
