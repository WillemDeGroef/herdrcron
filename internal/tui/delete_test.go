package tui

import (
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

func TestDeleteAsksThenDeletes(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksDelete, api.DeleteResult{ID: "peppol-sh-deps", StoppedRuns: []api.RunSummary{}})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "peppol-sh-deps")
	tm.Send(key("d"))
	waitText(t, tm, "Delete task peppol-sh-deps?")
	tm.Send(key("y"))
	got := waitCall(t, f, api.MethodTasksDelete, 1)
	if got[0] != (api.DeleteParams{ID: "peppol-sh-deps"}) {
		t.Fatalf("delete params = %+v", got[0])
	}
	waitText(t, tm, "deleted peppol-sh-deps")
	waitCall(t, f, api.MethodTasksList, 2)
	finalView(t, tm)
}

func TestDeleteNoKeepsTheTask(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	tm.Send(key("d"))
	waitText(t, tm, "delete task release-notes")
	tm.Send(key("n"))
	waitText(t, tm, "cancelled")
	finalView(t, tm)
	if n := len(f.callsOf(api.MethodTasksDelete)); n != 0 {
		t.Fatalf("%d deletes", n)
	}
}

func TestDeleteWithActiveRunOffersStopAndDelete(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksDelete, api.DeleteResult{ID: "factory-coordinator",
		StoppedRuns: []api.RunSummary{{ID: "r3", TaskID: "factory-coordinator", Status: "failed"}}})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("d"))
	waitText(t, tm, "Stop run r3 and delete task factory-coordinator?")
	tm.Send(key("y"))
	got := waitCall(t, f, api.MethodTasksDelete, 1)
	if got[0] != (api.DeleteParams{ID: "factory-coordinator", StopActive: true}) {
		t.Fatalf("delete params = %+v", got[0])
	}
	waitText(t, tm, "stopped run r3 and deleted factory-coordinator")
	finalView(t, tm)
}

func TestDeleteRefusedForARunThatStartedMeanwhile(t *testing.T) {
	f := fixture()
	f.fail(api.MethodTasksDelete, &api.Error{Code: api.CodeRunActiveDelete, Message: "task has an active run"})
	f.set(api.MethodTasksDelete, api.DeleteResult{ID: "peppol-sh-deps"})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "peppol-sh-deps")
	tm.Send(key("d"))
	waitText(t, tm, "Delete task peppol-sh-deps?")
	tm.Send(key("y"))
	waitText(t, tm, "peppol-sh-deps started a run meanwhile. Stop it and delete the task?")
	tm.Send(key("y"))
	got := waitCall(t, f, api.MethodTasksDelete, 2)
	if got[1] != (api.DeleteParams{ID: "peppol-sh-deps", StopActive: true}) {
		t.Fatalf("second delete params = %+v", got[1])
	}
	waitText(t, tm, "deleted peppol-sh-deps")
	finalView(t, tm)
}
