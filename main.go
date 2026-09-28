package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "ShadowChat:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("Usage:\n  shadowchat host --listen 127.0.0.1:9000 --name Orest\n  shadowchat join --address HOST:9000 --name Sophia --fingerprint SHA256")
		return nil
	}
	command := args[0]
	if command != "host" && command != "join" {
		return errors.New("expected host or join (use --help for usage)")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	name := flags.String("name", "", "nickname (1–24 ASCII letters, digits, underscores, or hyphens)")
	var listen, address, fingerprint string
	if command == "host" {
		flags.StringVar(&listen, "listen", "127.0.0.1:9000", "address and port to listen on")
	} else {
		flags.StringVar(&address, "address", "", "host address and port")
		flags.StringVar(&fingerprint, "fingerprint", "", "host's SHA-256 TLS certificate fingerprint (required)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := validateName(*name); err != nil {
		return fmt.Errorf("--name: %w", err)
	}
	if command == "join" {
		if address == "" {
			return errors.New("--address is required")
		}
		return runClient(ctx, address, fingerprint, *name)
	}
	host, fingerprint, err := startServer(listen)
	if err != nil {
		return err
	}
	defer host.close()
	listening := host.listener.Addr().String()
	localAddress, sampleAddress, wildcard := clientAddresses(listening)
	fmt.Printf("Listening on %s\nTLS SHA-256 fingerprint: %s\n", listening, fingerprint)
	fmt.Printf("Join: shadowchat join --address %s --name Sophia --fingerprint %s\n", sampleAddress, fingerprint)
	if wildcard {
		fmt.Println("Replace HOST_LAN_IP with this host's actual LAN IP address; do not connect to the wildcard listen address.")
	}
	fmt.Println("Share the fingerprint through a trusted channel. It changes every host restart. /quit ends the room.")
	return runClient(ctx, localAddress, fingerprint, *name)
}

func clientAddresses(listening string) (local, sample string, wildcard bool) {
	host, port, _ := net.SplitHostPort(listening)
	ip := net.ParseIP(host)
	if ip != nil && ip.IsUnspecified() {
		loopback := "127.0.0.1"
		if ip.To4() == nil {
			loopback = "::1"
		}
		return net.JoinHostPort(loopback, port), net.JoinHostPort("HOST_LAN_IP", port), true
	}
	return listening, listening, false
}
