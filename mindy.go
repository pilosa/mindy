// mindy is a Multi INDex proxY for Pilosa. It should be used with a Pilosa
// instance which has multiple indexes which have distinct columns, but their
// rows all mean the same thing. Its inputs are:
// 1. the set of indexes to query.
// 2. a list of row,frame pairs to include.
// 3. a list of row,frame pairs to exclude.
// 4. the conjunction (AND or OR) which determines whether it will be an Intersect or Union respectively.
package mindy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/pilosa/go-pilosa"
	"github.com/pkg/errors"
)

type Main struct {
	Pilosa []string `help:"Comma separated list of pilosa hosts/ports."`
	Bind   string   `help:"Host/port to bind to."`
}

func NewMain() *Main {
	return &Main{
		Pilosa: []string{"localhost:10101"},
		Bind:   ":10001",
	}
}

func (m *Main) Run() error {
	client, err := pilosa.NewClientFromAddresses(m.Pilosa, nil)
	if err != nil {
		return errors.Wrap(err, "creating Pilosa client")
	}
	h := &Handler{
		client: client,
		sem:    make(semaphore, 2), // length of semaphore is number of concurrent goroutines querying pilosa.
	}

	sm := http.NewServeMux()
	sm.HandleFunc("/mindy", h.handleMindy)
	s := &http.Server{
		Addr:    m.Bind,
		Handler: sm,
	}

	return s.ListenAndServe()
}

type Row struct {
	ID    uint64 `json:"id"`
	Frame string `json:"frame"`
}

type Request struct {
	Indexes     []string `json:"indexes"`
	Includes    []Row    `json:"includes"`
	Excludes    []Row    `json:"excludes"`
	Conjunction string   `json:"conjunction"`
}

type semaphore chan struct{}

func (s semaphore) Acquire() {
	s <- struct{}{}
}

func (s semaphore) Release() {
	<-s
}

type Handler struct {
	client *pilosa.Client
	sem    semaphore
}

func (h *Handler) handleMindy(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "must POST to /mindy", http.StatusMethodNotAllowed)
		return
	}
	dec := json.NewDecoder(r.Body)
	req := &Request{}
	err := dec.Decode(req)
	if err != nil {
		http.Error(w, "decoding: "+err.Error(), http.StatusBadRequest)
		return
	}

	results, err := h.Query(req)
	if err != nil {
		http.Error(w, "querying pilosa: "+err.Error(), http.StatusInternalServerError)
		return
	}

	enc := json.NewEncoder(w)
	err = enc.Encode(results)
	if err != nil {
		http.Error(w, "encoding response"+err.Error(), http.StatusInternalServerError)
		return
	}

}

type Results struct {
	Bits map[string][]uint64 `json:"bits"`
}

func (h *Handler) Query(r *Request) (*Results, error) {

	results := &Results{
		Bits: make(map[string][]uint64),
	}

	var eg errgroup.Group

	for _, i := range r.Indexes {
		i := i // required for closure
		eg.Go(func() error {
			bits, err := h.queryIndex(i, r)
			if err != nil {
				return err
			}
			results.Bits[i] = bits
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return results, nil
}

func (h *Handler) queryIndex(idx string, r *Request) ([]uint64, error) {
	h.sem.Acquire()
	defer h.sem.Release()

	schema, err := h.client.Schema()
	if err != nil {
		return nil, fmt.Errorf("getting schema: %v", err)
	}
	index, err := schema.Index(idx)
	if err != nil {
		return nil, fmt.Errorf("getting index %s from schema: %v", idx, err)
	}

	var includes []*pilosa.PQLBitmapQuery
	//var excludes []*pilosa.PQLBitmapQuery // TODO: implement excludes

	for _, row := range r.Includes {
		frame, err := index.Frame(row.Frame)
		if err != nil {
			return nil, fmt.Errorf("getting frame %s from index %s: %v", row.Frame, idx, err)
		}
		includes = append(includes, frame.Bitmap(row.ID))
	}

	var qry *pilosa.PQLBitmapQuery
	// TODO: only handle specific conjunctions. error on the others.
	if r.Conjunction == "and" {
		qry = index.Intersect(includes...)
	} else {
		qry = index.Union(includes...)
	}

	response, err := h.client.Query(qry)
	if err != nil {
		return nil, fmt.Errorf("querying index %s: %v", idx, err)
	}
	resp := response.ResultList[0] // TODO: check slice length
	bitmap := resp.Bitmap

	return bitmap.Bits, nil
}
