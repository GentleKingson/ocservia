// Local, authenticated administrative export. This binary stays on Controller.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/google/uuid"
)

func main() {
	backend := flag.String("backend", "postgres", "database backend")
	dsnFile := flag.String("database-url-file", "", "protected Controller database credential file")
	ca := flag.String("database-ca-file", "", "database trust CA")
	workspace := flag.String("workspace", "", "expected workspace UUID")
	node := flag.String("node", "", "expected node UUID")
	approval := flag.String("approval", "", "consumed enrollment approval UUID")
	endpoint := flag.String("endpoint", "", "expected endpoint hex identity")
	flag.Parse()
	fail := func() { fmt.Fprintln(os.Stderr, "approved sealing binding export rejected"); os.Exit(1) }
	info, err := os.Lstat(*dsnFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		fail()
	}
	dsn, err := os.ReadFile(*dsnFile)
	if err != nil {
		fail()
	}
	defer clear(dsn)
	w, e1 := uuid.Parse(*workspace)
	n, e2 := uuid.Parse(*node)
	a, e3 := uuid.Parse(*approval)
	if e1 != nil || e2 != nil || e3 != nil || w == uuid.Nil || n == uuid.Nil || a == uuid.Nil {
		fail()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := connection.Open(ctx, connection.Options{Backend: *backend, Environment: "production", URL: strings.TrimSpace(string(dsn)), CAFile: *ca})
	if err != nil {
		fail()
	}
	defer db.Close()
	data, err := enrollment.ExportSealingBinding(ctx, db.Store, w, n, a, *endpoint)
	if err != nil {
		fail()
	}
	if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
		fail()
	}
}
