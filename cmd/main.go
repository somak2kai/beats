package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/somak2kai/beats/pkg/ast"
	"github.com/somak2kai/beats/pkg/db"
	ds "github.com/somak2kai/beats/pkg/types"
	"github.com/somak2kai/beats/pkg/util"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync() //nolint

	prj := flag.String("repo", "", "Provide repository path")

	// Parse the flags from os.Args
	flag.Parse()
	base := filepath.Base(*prj)
	fmt.Println("proj repo", *prj)
	files, err := util.GetFileMetadata(*prj)
	if err != nil {
		log.Fatal("failed to get file metadata", zap.Error(err))
	}

	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	var fMeta []ds.FunctionMeta
	var mu sync.Mutex
	for _, val := range files {
		for _, m := range val {
			m := m
			g.Go(func() error {
				meta, err := ast.ParseFile(m)
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				fMeta = append(fMeta, meta...)
				return nil
			})
		}
	}

	if err := g.Wait(); err != nil {
		log.Fatal("unable to capture file metadata record", zap.Error(err))
		return
	}

	fmt.Println("total number of functions", len(fMeta))
	index := ds.PopulateIndex(fMeta)
	if err := dumpIndex(index, "/tmp/beats/index/index_"+base+".json"); err != nil {
		fmt.Println(err)
		return
	}
	if err := dumpFunctions(fMeta, "/tmp/beats/fmeta/fmeta_"+base+".json"); err != nil {
		fmt.Println(err)
		return
	}

	tmp := filepath.Join(os.TempDir(), "badger", base)
	fmt.Println(tmp)
	bDb := db.NewDb(tmp)
	for k, v := range index.Postings {
		if err := bDb.StorePostings(k, v); err != nil {
			log.Fatal("unable to save inverted index", zap.Error(err))
		}
	}

	for k, v := range index.DocFreq {
		if err := bDb.StoreDocFreq(k, v); err != nil {
			log.Fatal("unable to save document frequency", zap.Error(err))
		}
	}

	for k, v := range index.FuncMeta {
		if err := bDb.StoreFunctionMeta(k, v); err != nil {
			log.Fatal("unable to save function metadata", zap.Error(err))
		}
	}
	clusters := ast.BuildClusters(fMeta)
	collapsed := ast.CollapseToFamilies(clusters)

	// what you likely want immediately after:
	labelable := ast.Labelable(collapsed, 0.60, 4)
	fmt.Printf("total clusters: %d, labelable: %d, primitive (filtered): %d\n",
		len(clusters),
		len(labelable),
		func() int {
			n := 0
			for _, c := range clusters {
				if c.IsPrimitive {
					n++
				}
			}
			return n
		}(),
	)

	if err := dumpClusters(collapsed, "/tmp/beats/fmeta/cluster_"+base+".json"); err != nil {
		fmt.Println(err)
		return
	}

	beatsDir := filepath.Join(*prj, ".beats")
	if err := os.MkdirAll(beatsDir, 0755); err != nil {
		log.Fatal("unable to create .beats directory", zap.Error(err))
	}

	labelFile := filepath.Join(beatsDir, "beats_label_"+base+".md")
	if err := createClusterLabels(labelable, labelFile, base); err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println("cluster file written to", labelFile)
}

func createClusterLabels(cls []ds.Cluster, path, repo string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return ast.WriteClusters(f, repo, cls)
}
func dumpClusters(cls []ds.Cluster, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, fn := range cls {
		if err := enc.Encode(fn); err != nil {
			return err
		}
	}
	return nil
}

func dumpFunctions(fMeta []ds.FunctionMeta, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, fn := range fMeta {
		if err := enc.Encode(fn); err != nil {
			return err
		}
	}
	return nil
}

func dumpIndex(index ds.Index, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(index)
}
