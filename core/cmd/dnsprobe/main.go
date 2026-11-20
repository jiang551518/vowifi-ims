// dnsprobe sends a raw DNS A query: dnsprobe [-n name] server...
package main

import (
	"flag"
	"fmt"
	"net"
	"strings"
	"time"
)

func main() {
	name := flag.String("n", "www.baidu.com", "name to query")
	flag.Parse()
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(*name, ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	for _, s := range flag.Args() {
		start := time.Now()
		c, err := net.Dial("udp", s)
		if err != nil {
			fmt.Printf("%-24s dial error %v\n", s, err)
			continue
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write(q)
		b := make([]byte, 1500)
		n, err := c.Read(b)
		c.Close()
		if err != nil {
			fmt.Printf("%-24s FAIL %v\n", s, err)
			continue
		}
		// Print the first A record if present (answers start after the question).
		var ip string
		if n >= 16 && b[n-6] == 0 && b[n-5] == 4 {
			ip = net.IP(b[n-4 : n]).String()
		}
		fmt.Printf("%-24s %-28s rcode=%d answers=%d first=%s %v\n", s, *name, b[3]&0xf, int(b[6])<<8|int(b[7]), ip, time.Since(start).Round(time.Millisecond))
	}
}
