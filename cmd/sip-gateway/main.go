package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/debt-flow/sip-gateway/internal/config"
	"github.com/debt-flow/sip-gateway/internal/diagnostics"
	"github.com/debt-flow/sip-gateway/internal/gateway"
	"github.com/debt-flow/sip-gateway/internal/service"
)

var version = "dev"

func main() {
	defaultConfig := "config.yaml"
	if runtime.GOOS == "windows" {
		defaultConfig = `C:\ProgramData\DebtFlow\SipGateway\config.yaml`
	}
	configPath := flag.String("config", defaultConfig, "path to gateway YAML configuration")
	checkConfig := flag.Bool("check-config", false, "validate configuration and exit")
	diagnosticsPath := flag.String("write-diagnostics", "", "write a sanitized diagnostics report and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("configuration error: %v", err)
		os.Exit(2)
	}
	if *checkConfig {
		fmt.Println("configuration is valid")
		return
	}
	if *diagnosticsPath != "" {
		if err := diagnostics.Write(*diagnosticsPath, cfg, version, time.Now()); err != nil {
			log.Printf("write diagnostics: %v", err)
			os.Exit(2)
		}
		fmt.Printf("sanitized diagnostics written to %s\n", *diagnosticsPath)
		return
	}
	if err := service.Run("DebtFlowSipGateway", func(ctx context.Context) error {
		return gateway.RunWithVersion(ctx, cfg, version)
	}); err != nil {
		log.Printf("gateway stopped with error: %v", err)
		os.Exit(1)
	}
}
