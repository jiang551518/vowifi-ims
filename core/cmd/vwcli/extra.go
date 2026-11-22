package main

import (
	"context"
	"encoding/json"
	"fmt"

	"vowifi/sim"
)

func runExtra(ctx context.Context, h *sim.Helper, args []string) error {
	switch args[0] {
	case "esim":
		return cmdEsim(ctx, h, args)
	case "call":
		return cmdCall(ctx, h, args[1:])
	case "ims":
		return cmdIMS(ctx, h, args[1:])
	case "connect":
		return cmdConnect(ctx, h, args[1:])
	case "netsel":
		// netsel <subId> block|auto
		var resp map[string]any
		var sub int
		fmt.Sscanf(args[1], "%d", &sub)
		err := h.Call(ctx, "netsel", map[string]any{"subId": sub, "block": args[2] == "block"}, &resp)
		fmt.Println(resp["result"], err)
		return nil
	case "nettypes":
		// nettypes <subId> <reason> [mask]
		var resp map[string]any
		var sub, reason int
		fmt.Sscanf(args[1], "%d", &sub)
		fmt.Sscanf(args[2], "%d", &reason)
		req := map[string]any{"subId": sub, "reason": reason}
		if len(args) > 3 {
			var m int64
			fmt.Sscanf(args[3], "%d", &m)
			req["mask"] = m
		}
		err := h.Call(ctx, "nettypes", req, &resp)
		fmt.Println(resp["result"], err)
		return nil
	case "svcstate":
		// svcstate <slot>  ->  voice,data,manual
		var resp map[string]any
		var slot int
		fmt.Sscanf(args[1], "%d", &slot)
		err := h.Call(ctx, "svcstate", map[string]any{"slot": slot}, &resp)
		fmt.Println(resp["result"], err)
		return nil
	case "cellinfo":
		// cellinfo <slot> <subId>  ->  JSON snapshot (network, band, signal)
		var resp map[string]any
		var slot, sub int
		fmt.Sscanf(args[1], "%d", &slot)
		fmt.Sscanf(args[2], "%d", &sub)
		err := h.Call(ctx, "cellinfo", map[string]any{"slot": slot, "subId": sub}, &resp)
		fmt.Println(resp["result"], err)
		return nil
	case "methods":
		var resp map[string]any
		err := h.Call(ctx, "methods", map[string]any{"service": args[1], "filter": args[2]}, &resp)
		b, _ := json.MarshalIndent(resp["methods"], "", "  ")
		fmt.Println(string(b), err)
		return nil
	case "classinfo":
		var resp map[string]any
		err := h.Call(ctx, "classinfo", map[string]any{"class": args[1]}, &resp)
		b, _ := json.MarshalIndent(resp["methods"], "", "  ")
		fmt.Println(string(b), err)
		return nil
	case "invoke":
		// invoke <service> <method> <json-args-array>
		var a []any
		if err := json.Unmarshal([]byte(args[3]), &a); err != nil {
			return err
		}
		for i, v := range a {
			if f, ok := v.(float64); ok {
				a[i] = int(f)
			}
		}
		var resp map[string]any
		err := h.Call(ctx, "invoke", map[string]any{"service": args[1], "method": args[2], "args": a}, &resp)
		fmt.Println(resp["result"], err)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
