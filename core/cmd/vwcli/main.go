// vwcli is a developer CLI that runs on the phone (as root, via adb) to
// exercise the VoWiFi core without the Android UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"vowifi/sim"
)

var helperPath = flag.String("helper", "/data/local/tmp/vwhelper.jar", "RootHelper classpath")

func main() {
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	h, err := sim.StartHelper(ctx, *helperPath, "")
	if err != nil {
		fail(err)
	}
	defer h.Close()
	h.Log = func(s string) { fmt.Fprintln(os.Stderr, s) }
	args := flag.Args()
	if len(args) == 0 {
		args = []string{"subs"}
	}
	if err := run(ctx, h, args); err != nil {
		fail(err)
	}
}

func run(ctx context.Context, h *sim.Helper, args []string) error {
	switch args[0] {
	case "subs":
		subs, err := h.Subscriptions(ctx)
		if err != nil {
			return err
		}
		dump(subs)
	case "cards":
		cards, err := h.Cards(ctx)
		if err != nil {
			return err
		}
		dump(cards)
	case "id":
		subID, slot := atoi(args, 1), atoi(args, 2)
		id, err := h.Identity(ctx, subID, slot)
		if err != nil {
			return err
		}
		dump(id)
	case "aka":
		// Random challenge: the card must reject AUTN (MAC failure), which
		// proves the request reached the UICC application.
		subID, app := atoi(args, 1), atoi(args, 2)
		r, a := make([]byte, 16), make([]byte, 16)
		_, _ = rand.Read(r)
		_, _ = rand.Read(a)
		res, err := h.AKA(ctx, subID, app, r, a)
		if errors.Is(err, sim.ErrSyncFailure) {
			fmt.Println("sync failure, AUTS len", len(res.AUTS))
			return nil
		}
		fmt.Printf("result: res=%d ck=%d ik=%d err=%v\n", len(res.RES), len(res.CK), len(res.IK), err)
	default:
		return runExtra(ctx, h, args)
	}
	return nil
}

func atoi(args []string, i int) int {
	if i >= len(args) {
		return -1
	}
	n, err := strconv.Atoi(args[i])
	if err != nil {
		fail(err)
	}
	return n
}

func dump(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
