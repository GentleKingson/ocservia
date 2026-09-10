package mysql

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	driver "github.com/go-sql-driver/mysql"
)

func TestConnectionPolicy(t *testing.T) {
	base := Options{Engine: MySQL, Environment: "test", DSN: "user:secret@tcp(127.0.0.1:3306)/ocservia?tls=false"}
	c, err := configuration(base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 5*time.Second || c.ReadTimeout != 30*time.Second || c.WriteTimeout != 30*time.Second || c.Loc != time.UTC || !c.ParseTime || c.Params["time_zone"] != "'+00:00'" || c.Params["transaction_isolation"] != "'READ-COMMITTED'" {
		t.Fatal("connection policy incomplete")
	}
	for _, suffix := range []string{"", "?tls=skip-verify", "?tls=preferred", "?tls=false&multiStatements=true", "?tls=false&time_zone=%27SYSTEM%27", "?tls=false&allowAllFiles=true", "?tls=false&tls=true", "?tls=false&timeout=0"} {
		o := base
		o.DSN = "user:secret@tcp(127.0.0.1:3306)/ocservia" + suffix
		if _, err := configuration(o); err == nil {
			t.Fatalf("accepted unsafe parameters %s", suffix)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("secret leak")
		}
	}
	for _, environment := range []string{"production", "", "staging"} {
		o := base
		o.Environment = environment
		if _, err := configuration(o); err == nil {
			t.Fatal("environment gate")
		}
	}
	o := base
	o.Engine = "auto"
	if _, err := configuration(o); err == nil {
		t.Fatal("engine inference")
	}
	o = base
	o.DSN = "user:secret@tcp(db.internal:3306)/ocservia?tls=false"
	if _, err := configuration(o); err == nil {
		t.Fatal("remote plaintext")
	}
	o.DSN = "user:secret@tcp(db.internal:3306)/ocservia?tls=true"
	c, err = configuration(o)
	if err != nil || c.TLS == nil || c.TLS.InsecureSkipVerify || c.TLS.ServerName != "db.internal" {
		t.Fatal("TLS verification missing")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", base, base), "secret") {
		t.Fatal("options formatting leaked DSN")
	}
}

func TestSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		code uint16
		kind error
	}{{1062, database.ErrUnique}, {1452, database.ErrForeignKey}, {3819, database.ErrConstraint}, {1213, database.ErrDeadlock}, {1143, database.ErrPermission}} {
		err := safeError(&driver.MySQLError{Number: tc.code, Message: "private value"})
		if !errors.Is(err, tc.kind) || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe error classification")
		}
	}
	if !errors.Is(safeError(context.Canceled), context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if strings.Contains(safeError(errors.New("password=private")).Error(), "private") {
		t.Fatal("unknown error leak")
	}
}

func TestPinnedManifests(t *testing.T) {
	for _, engine := range []Engine{MySQL, MariaDB} {
		m, _, err := loadManifest(engine)
		if err != nil {
			t.Fatal(err)
		}
		tables := 0
		for _, s := range m.Steps {
			if s.Kind == "table" {
				tables++
			}
			if len(s.SchemaHash) != 64 {
				t.Fatalf("missing schema fingerprint %s/%s", engine, s.Name)
			}
		}
		if tables != 70 {
			t.Fatalf("baseline has %d tables", tables)
		}
	}
}

func TestValueCodecs(t *testing.T) {
	for _, literal := range []string{"192.0.2.123/24", "2001:db8::123/64", "::ffff:192.0.2.1/128"} {
		prefix := netip.MustParsePrefix(literal)
		encoded, err := InetBytes(prefix)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ParseInet(encoded)
		if err != nil || decoded != prefix {
			t.Fatal("inet host bits/family changed")
		}
	}
	if _, err := ParseInet([]byte{4, 33, 0, 0, 0, 0}); err == nil {
		t.Fatal("invalid prefix accepted")
	}
	var array TextArray
	value, err := array.Value()
	if err != nil || value != nil {
		t.Fatal("SQL NULL lost")
	}
	if err = array.Scan([]byte(`[]`)); err != nil || array == nil || len(array) != 0 {
		t.Fatal("empty array lost")
	}
	if err = array.Scan([]byte(`["text",null,""]`)); err != nil || len(array) != 3 || array[1] != nil || *array[2] != "" {
		t.Fatal("nullable elements changed")
	}
	if err = array.Scan([]byte(`[["text"]]`)); err == nil {
		t.Fatal("multidimensional array silently flattened")
	}
}
