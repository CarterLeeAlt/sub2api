package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	_ "github.com/lib/pq"
)

func main() {
	accountID := flag.Int64("account-id", 0, "OpenAI OAuth account ID")
	startText := flag.String("start", "", "seat change time (RFC3339)")
	endText := flag.String("weekly-end", "", "expected end of the affected weekly window (RFC3339)")
	evidence := flag.String("evidence", "", "source of the confirmed seat change time")
	estimated := flag.Bool("estimated", false, "the seat change time is estimated")
	apply := flag.Bool("apply", false, "record the change (default is a read-only preview)")
	flag.Parse()

	start, err := time.Parse(time.RFC3339Nano, *startText)
	if err != nil {
		log.Fatalf("invalid --start: %v", err)
	}
	end, err := time.Parse(time.RFC3339Nano, *endText)
	if err != nil {
		log.Fatalf("invalid --weekly-end: %v", err)
	}
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("postgres", cfg.Database.DSN())
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := repository.RecordOpenAIWeeklySeatChange(ctx, db, repository.OpenAIWeeklySeatChange{
		AccountID: *accountID, Start: start, ExpectedWeeklyEnd: end, Evidence: *evidence, Estimated: *estimated,
	}, *apply)
	if err != nil {
		log.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		log.Fatal(err)
	}
}
