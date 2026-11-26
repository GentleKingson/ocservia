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
	dsnFile := flag.String("database-url-file", "", "protected Controller database credential file")
	ca := flag.String("database-ca-file", "", "database trust CA")
	bundled := flag.Bool("bundled-postgres", false, "the URL targets the bundled PostgreSQL service")
	workspace := flag.String("workspace", "", "expected workspace UUID")
	node := flag.String("node", "", "expected node UUID")
	endpoint := flag.String("endpoint", "", "expected endpoint hex identity")
	flag.Parse()
	fail := func() { fmt.Fprintln(os.Stderr, "sealing binding export rejected"); os.Exit(1) }
	dsn, err := readCredentialFile(*dsnFile)
	if err != nil {
		fail()
	}
	defer clear(dsn)
	w, e1 := uuid.Parse(*workspace)
	n, e2 := uuid.Parse(*node)
	if e1 != nil || e2 != nil || w == uuid.Nil || n == uuid.Nil {
		fail()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := connection.Open(ctx, connection.Options{Environment: "production", URL: strings.TrimSpace(string(dsn)), CAFile: *ca, BundledPostgres: *bundled})
	if err != nil {
		fail()
	}
	defer db.Close()
	data, err := enrollment.ExportSealingBinding(ctx, db.Store, w, n, *endpoint)
	if err != nil {
		fail()
	}
	if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
		fail()
	}
}
