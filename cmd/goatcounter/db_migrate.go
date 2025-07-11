package main

import (
	"fmt"
	"slices"
	"strings"

	"zgo.at/errors"
	"zgo.at/goatcounter/v2"
	"zgo.at/goatcounter/v2/db/migrate/gomig"
	"zgo.at/goatcounter/v2/pkg/log"
	"zgo.at/zdb"
	"zgo.at/zli"
	"zgo.at/zstd/zfs"
	"zgo.at/zstd/zslice"
)

func cmdDBMigrate(f zli.Flags, dbConnect *string, debug []string, createdb *bool) error {
	var (
		dev  = f.Bool(false, "dev")
		test = f.Bool(false, "test")
		show = f.Bool(false, "show")
	)
	if err := f.Parse(zli.FromEnv("GOATCOUNTER")); err != nil && !errors.As(err, &zli.ErrUnknownEnv{}) {
		return err
	}

	if len(f.Args) == 0 {
		return errors.New("need a migration or command")
	}

	log.SetDebug(debug)

	db, _, err := connectDB(*dbConnect, "", nil, *createdb, false)
	if err != nil {
		return err
	}
	defer db.Close()

	fsys, err := zfs.EmbedOrDir(goatcounter.DB, "", dev.Bool())
	if err != nil {
		return err
	}
	m, err := zdb.NewMigrate(db, fsys, gomig.Migrations)
	if err != nil {
		return err
	}

	m.Test(test.Bool())
	m.Show(show.Set())

	if zslice.ContainsAny(f.Args, "pending", "list") {
		have, ran, err := m.List()
		if err != nil {
			return err
		}
		diff := zslice.Difference(have, ran)
		pending := "no pending migrations"
		if len(diff) > 0 {
			pending = fmt.Sprintf("pending migrations:\n\t%s", strings.Join(diff, "\n\t"))
		}

		if slices.Contains(f.Args, "list") {
			for i := range have {
				if slices.Contains(diff, have[i]) {
					have[i] = "pending: " + have[i]
				}
			}
			fmt.Fprintln(zli.Stdout, strings.Join(have, "\n"))
			return nil
		}

		if len(diff) > 0 {
			return errors.New(pending)
		}
		fmt.Fprintln(zli.Stdout, pending)
		return nil
	}

	return m.Run(f.Args...)
}
