package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"vowifi/esim"
	"vowifi/sim"
)

// esim <subId> <slot>: read eUICC info and profiles.
func cmdEsim(ctx context.Context, h *sim.Helper, args []string) error {
	subID, _ := strconv.Atoi(args[1])
	slot, _ := strconv.Atoi(args[2])
	card, err := esim.Open(ctx, h, sim.ChannelTarget{SubID: subID, Slot: slot})
	if err != nil {
		return err
	}
	defer card.Close(ctx)
	fmt.Println("ISD-R:", card.AIDName)
	info, err := card.ReadInfo(ctx)
	b, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(b), err)
	ps, err := card.Profiles(ctx)
	b, _ = json.MarshalIndent(ps, "", "  ")
	fmt.Println(string(b), err)
	return nil
}
