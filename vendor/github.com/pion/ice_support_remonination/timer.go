package ice

import (
	"container/heap"
	"runtime"
	"sync/atomic"
	"time"
)

type TaskHeap []TimerTask

func (t TaskHeap) Len() int           { return len(t) }
func (t TaskHeap) Less(i, j int) bool { return t[i].delta.Before(t[j].delta) }
func (t TaskHeap) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }

func (h *TaskHeap) Push(x interface{}) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*h = append(*h, x.(TimerTask))
}

func (h *TaskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

type TimerTask struct {
	delta time.Time
	task  func()
}

type TimerInterface interface {
	DelayTimerTask(timeout time.Duration, f func())
	RunInWorker(f func())
}

type TimerWorker struct {
	taskChan   chan func()
	timerQueue TaskHeap
	heapChan   chan TimerTask
}
type Timer struct {
	rrCount uint32
	worker  []TimerInterface
}

var globalTimer Timer

func init() {
	globalTimer.worker = make([]TimerInterface, runtime.NumCPU())

	for i := range globalTimer.worker {
		worker := &TimerWorker{}
		worker.taskChan = make(chan func())
		worker.timerQueue = make([]TimerTask, 0)
		heap.Init(&worker.timerQueue)
		worker.heapChan = make(chan TimerTask, 20480)
		globalTimer.worker[i] = worker

		go worker.mainLoop()
	}

}
func (t *Timer) GetRandomWorker() TimerInterface {

	res := atomic.AddUint32(&t.rrCount, 1)
	return t.worker[res%uint32(len(t.worker))]
}

func (t *TimerWorker) RunInWorker(f func()) {
	t.taskChan <- f
}

//time task maybe runned after closed, do check done channel
func (t *TimerWorker) DelayTimerTask(timeout time.Duration, f func()) {
	t.heapChan <- TimerTask{delta: time.Now().Add(timeout), task: f}
}

func (t *TimerWorker) mainLoop() {
	ticker := time.NewTimer(time.Second)
	ticker.Stop()

	for {
		select {
		case timerTask := <-t.heapChan:
			heap.Push(&t.timerQueue, timerTask)
			task := heap.Pop(&t.timerQueue)
			nearestTask := task.(TimerTask)

			delta := -time.Since(nearestTask.delta)
			if delta < 0 {
				delta = 0
			}

			ticker = time.NewTimer(delta)
			heap.Push(&t.timerQueue, task)
		default:
			select {
			case <-ticker.C:
				if len(t.timerQueue) == 0 {
					break
				}
				timerTask := heap.Pop(&t.timerQueue)
				task := timerTask.(TimerTask)

				task.task()

				if len(t.timerQueue) == 0 {
					break
				}
				timerTask = heap.Pop(&t.timerQueue)
				task = timerTask.(TimerTask)

				d := -time.Since(task.delta)
				if d < 0 {
					d = 0
				}
				ticker = time.NewTimer(d)
				heap.Push(&t.timerQueue, timerTask)

			case normalTask := <-t.taskChan:
				normalTask()
			}
		}

	}

}

type TimeFromNow interface {
	GetTimeBeforeNow(duration time.Duration) time.Time
}

type timeBeforeNow struct{}

func (t *timeBeforeNow) GetTimeBeforeNow(duration time.Duration) time.Time {
	return time.Now().Add(-duration)
}
