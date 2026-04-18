package main

import (
	"context"
	"fmt"
	"os"
)

func usage() {
	fmt.Fprint(os.Stderr, mainHelp)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit()
	case "commit":
		err = cmdCommit(os.Args[2:])
	case "checkout":
		err = cmdCheckout(os.Args[2:])
	case "status":
		err = cmdStatus()
	case "log":
		err = cmdLog(os.Args[2:])
	case "head":
		err = cmdHead()
	case "remote":
		err = cmdRemote(os.Args[2:])
	case "push":
		err = cmdPush(ctx, os.Args[2:])
	case "pull":
		err = cmdPull(ctx, os.Args[2:])
	case "key":
		err = cmdKey(ctx, os.Args[2:])
	case "-h", "--help":
		fmt.Print(mainHelp)
		return
	case "help":
		err = cmdHelp(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
