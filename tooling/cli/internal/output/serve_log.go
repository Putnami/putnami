package output

import "go.putnami.dev/cli/model/jobs"

// serveLogRenderer is the jsonl renderer of a supervised serve run
// (Config.ServeLog): the machine stream on stdout, exactly the one the run
// would write without it, and the run's tasks rendered as `putnami serve`
// renders them in text mode on the serve log writer. A supervisor reads the
// session stream for its typed claims and passes the text through, so the
// workloads' output stays readable whatever the stream's budget elides. The
// serve log carries no session summary, such as an opening line or the counts:
// the supervisor renders its own session around it
// (TextRendererConfig.TasksOnly).
//
// Only the jsonl stream fans out: an aggregated json envelope is written once,
// at Finish, too late for a workload's live output.
//
// It embeds the machine renderer, so every capability the session recording
// and the scheduler look for (configureMachineSession, SetPublishConcurrency,
// RecordSessionEvent, the batch events) is the machine renderer's own. Only the
// five methods a text renderer implements fan out, the machine record first.
type serveLogRenderer struct {
	*machineV2Renderer
	text *TextRenderer
}

func newServeLogRenderer(machine *machineV2Renderer, cfg Config) *serveLogRenderer {
	return &serveLogRenderer{
		machineV2Renderer: machine,
		text: NewTextRenderer(cfg.ServeLog, cfg.ServeLog, TextRendererConfig{
			Verbose:   cfg.Verbose,
			Debug:     cfg.Debug,
			Quiet:     cfg.Quiet,
			ServeMode: true,
			TasksOnly: true,
		}),
	}
}

// Start starts both renderings.
func (r *serveLogRenderer) Start(planned []*jobs.ScheduledJob) {
	r.machineV2Renderer.Start(planned)
	r.text.Start(planned)
}

// JobStart renders a task's start on both.
func (r *serveLogRenderer) JobStart(job *jobs.ScheduledJob) {
	r.machineV2Renderer.JobStart(job)
	r.text.JobStart(job)
}

// JobEvent renders one event on both: a task:event record, and a serve log
// line for a log event.
func (r *serveLogRenderer) JobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	r.machineV2Renderer.JobEvent(job, event)
	r.text.JobEvent(job, event)
}

// JobComplete renders a task's end on both, with a failed task's details on
// the serve log writer.
func (r *serveLogRenderer) JobComplete(job *jobs.ScheduledJob, result *jobs.JobResult) {
	r.machineV2Renderer.JobComplete(job, result)
	r.text.JobComplete(job, result)
}

// Finish ends both renderings: session:end on the stream; the serve log, which
// carries the tasks alone, writes nothing more.
func (r *serveLogRenderer) Finish(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	r.machineV2Renderer.Finish(results, outcome)
	r.text.Finish(results, outcome)
}
