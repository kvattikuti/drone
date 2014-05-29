package queue

import (
	"github.com/drone/drone/pkg/build/script"
	. "github.com/drone/drone/pkg/model"
)

// A Queue dispatches tasks to workers.
type Queue struct {
	tasks chan<- *BuildTask
}

// BuildTasks represents a build that is pending
// execution.
type BuildTask struct {
	Repo   *Repo
	Commit *Commit
	Build  *Build

	// Build instructions from the .drone.yml
	// file, unmarshalled.
	Script *script.Build
}

// Start N workers with the given build runner.
func Start(workers int, runner BuildRunner) *Queue {
	tasks := make(chan *BuildTask)

	queue := &Queue{tasks: tasks}
	println("number of work threads => ", workers)
	for i := 0; i < workers; i++ {
		worker := worker{
			runner: runner,
		}

		go worker.work(tasks)
	}

	return queue
}

// Add adds the task to the build queue.
func (q *Queue) Add(task *BuildTask) {
	q.tasks <- task
}
