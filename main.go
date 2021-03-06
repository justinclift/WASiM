package main

import (
	"bytes"
	"io/ioutil"
	"log"
	"os"
	"time"

	"github.com/go-interpreter/wagon/exec"
	"github.com/go-interpreter/wagon/wasm"
	"github.com/jackc/pgx"
)

type PrologueInfo struct {
	File int
	Line int
	Col  int
}

var (
	// Just while developing, to allow skipping past the DWARF debug info disassembly step
	disassembleDwarf = false

	// Yes, using globals for this is ugly.  But it's also super simple, so suitable for learning. ;)
	vm *exec.VM
)

const (
	FILE_UNKNOWN = 9999999 - iota
	FILE_STDIN
	FILE_STDOUT
	FILE_STDERR
)

func main() {
	// Load the wasm file containing DWARF debug info
	var err error
	if len(os.Args) != 2 {
		log.Fatal("Needs the .wasm file name to run, given on the command line")
	}
	raw, err := ioutil.ReadFile(os.Args[1]) // Yes, this could be done a lot better ;)
	if err != nil {
		panic(err)
	}

	// Connect to the database
	cfg := pgx.ConnConfig{
		Host:      "/tmp",
		User:      "jc",
		Database:  "wasim",
		TLSConfig: nil,
	}

	pgPoolConfig := pgx.ConnPoolConfig{cfg, 45, nil, 5 * time.Second}
	pg, err := pgx.NewConnPool(pgPoolConfig)
	if err != nil {
		panic(err)
	}

	// Grab the next available execution_run number
	var dbRun int
	dbQuery := `SELECT nextval('execution_runs_seq')`
	err = pg.QueryRow(dbQuery).Scan(&dbRun)
	if err != nil {
		log.Fatalf("retrieving next execution run number failed: %v\n", err)
	}
	log.Printf("opLog execution run: %d\n", dbRun)

	// Parse the wasm file
	m, err := wasm.ReadModule(bytes.NewReader(raw), funcResolver)
	if err != nil {
		panic(err)
	}

	// Construct a DWARF object from the section data
	if disassembleDwarf {
		err = parseDwarf(m)
		if err != nil {
			log.Fatal(err)
		}
	}

	// Add the function info to the database
	for i, j := range m.FunctionIndexSpace {
		dbQuery := `
			INSERT INTO execution_run_functions (run_num, function_num, function_name, num_returns, num_params)
			VALUES ($1, $2, $3, $4, $5)`
		var commandTag pgx.CommandTag
		commandTag, err = pg.Exec(dbQuery, dbRun, i, j.Name, len(j.Sig.ReturnTypes), len(j.Sig.ParamTypes))
		if err != nil {
			log.Print(err)
			return
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf("Wrong number of rows (%v) affected when adding a function name to the database: %s\n", numRows, j.Name)
		}
	}

	// Construct the wasm VM
	vm, err = exec.NewVM(m, exec.PGConnPool(pg), exec.PGDBRun(dbRun))
	if err != nil {
		log.Fatalf("could not create wasm vm: %v", err)
	}

	// Locate the main function to run
	foundMain := false
	mainID := uint32(0)
	for name, entry := range m.Export.Entries {
		if (name == "main" && entry.FieldStr == "main") || (name == "cwa_main" && entry.FieldStr == "cwa_main") {
			mainID = entry.Index
			foundMain = true
			break
		}
	}
	if !foundMain {
		panic("no main function found")
	}

	// Run the main function
	_, err = vm.ExecCode(int64(mainID))
	if err != nil {
		panic(err)
	}
}
