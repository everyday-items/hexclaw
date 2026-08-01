package assembly

import "testing"

func TestWireBuildsProductionTextbookCatalogWorker(t *testing.T) {
	runtime, err := Wire(newDB(t), fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.CatalogWorker == nil {
		t.Fatal("production K12 assembly omitted the durable textbook catalog worker")
	}
}
