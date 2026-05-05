package main

import (
	"context"
	"fmt"
	"os"

	"ec2cp/internal/cli"
	"ec2cp/internal/config"
)

func main() {
	config.LoadDotenv()
	if err := cli.NewRoot().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
