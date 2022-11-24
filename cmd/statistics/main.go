package main

import (
	"eth2-exporter/db"
	"eth2-exporter/services"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"eth2-exporter/version"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/sirupsen/logrus"
)

type options struct {
	configPath                string
	statisticsDayToExport     int64
	statisticsDaysToExport    string
	poolsDisabledFlag         bool
	statisticsValidatorToggle bool
	statisticsChartToggle     bool
}

var opt options = options{}

func main() {
	configPath := flag.String("config", "", "Path to the config file")
	statisticsDayToExport := flag.Int64("statistics.day", -1, "Day to export statistics (will export the day independent if it has been already exported or not")
	statisticsDaysToExport := flag.String("statistics.days", "", "Days to export statistics (will export the day independent if it has been already exported or not")
	poolsDisabledFlag := flag.Bool("pools.disabled", false, "Disable exporting pools")
	statisticsValidatorToggle := flag.Bool("validators.enabled", false, "Toggle exporting validator statistics")
	statisticsChartToggle := flag.Bool("charts.enabled", false, "Toggle exporting chart series")

	flag.Parse()

	opt = options{
		configPath:                *configPath,
		statisticsDayToExport:     *statisticsDayToExport,
		statisticsDaysToExport:    *statisticsDaysToExport,
		statisticsValidatorToggle: *statisticsChartToggle,
		poolsDisabledFlag:         *poolsDisabledFlag,
	}

	logrus.Printf("version: %v, config file path: %v", version.Version, *configPath)
	cfg := &types.Config{}
	err := utils.ReadConfig(cfg, *configPath)

	if err != nil {
		logrus.Fatalf("error reading config file: %v", err)
	}
	utils.Config = cfg

	db.MustInitDB(&types.DatabaseConfig{
		Username: cfg.WriterDatabase.Username,
		Password: cfg.WriterDatabase.Password,
		Name:     cfg.WriterDatabase.Name,
		Host:     cfg.WriterDatabase.Host,
		Port:     cfg.WriterDatabase.Port,
	}, &types.DatabaseConfig{
		Username: cfg.ReaderDatabase.Username,
		Password: cfg.ReaderDatabase.Password,
		Name:     cfg.ReaderDatabase.Name,
		Host:     cfg.ReaderDatabase.Host,
		Port:     cfg.ReaderDatabase.Port,
	})
	defer db.ReaderDb.Close()
	defer db.WriterDb.Close()

	db.MustInitFrontendDB(&types.DatabaseConfig{
		Username: cfg.Frontend.WriterDatabase.Username,
		Password: cfg.Frontend.WriterDatabase.Password,
		Name:     cfg.Frontend.WriterDatabase.Name,
		Host:     cfg.Frontend.WriterDatabase.Host,
		Port:     cfg.Frontend.WriterDatabase.Port,
	}, &types.DatabaseConfig{
		Username: cfg.Frontend.ReaderDatabase.Username,
		Password: cfg.Frontend.ReaderDatabase.Password,
		Name:     cfg.Frontend.ReaderDatabase.Name,
		Host:     cfg.Frontend.ReaderDatabase.Host,
		Port:     cfg.Frontend.ReaderDatabase.Port,
	})
	defer db.FrontendReaderDB.Close()
	defer db.FrontendWriterDB.Close()

	db.InitBigtable(cfg.Bigtable.Project, cfg.Bigtable.Instance, fmt.Sprintf("%d", utils.Config.Chain.Config.DepositChainID))

	if *statisticsDaysToExport != "" {
		s := strings.Split(*statisticsDaysToExport, "-")
		if len(s) < 2 {
			logrus.Fatalf("invalid arg")
		}
		firstDay, err := strconv.ParseUint(s[0], 10, 64)
		if err != nil {
			logrus.Fatal(err)
		}
		lastDay, err := strconv.ParseUint(s[1], 10, 64)
		if err != nil {
			logrus.Fatal(err)
		}

		if *statisticsValidatorToggle {
			logrus.Infof("exporting validator statistics for days %v-%v", firstDay, lastDay)
			for d := firstDay; d <= lastDay; d++ {
				_, err := db.WriterDb.Exec("delete from validator_stats_status where day = $1", d)
				if err != nil {
					logrus.Fatalf("error resetting status for day %v: %v", d, err)
				}

				err = db.WriteValidatorStatisticsForDay(uint64(d))
				if err != nil {
					logrus.Errorf("error exporting stats for day %v: %v", d, err)
				}
			}
		}

		if *statisticsChartToggle {
			logrus.Infof("exporting chart series for days %v-%v", firstDay, lastDay)
			for d := firstDay; d <= lastDay; d++ {
				_, err = db.WriterDb.Exec("delete from chart_series_status where day = $1", d)
				if err != nil {
					logrus.Fatalf("error resetting status for chart series status for day %v: %v", d, err)
				}

				err = db.WriteChartSeriesForDay(int64(d))
				if err != nil {
					logrus.Errorf("error exporting chart series from day %v: %v", d, err)
				}
			}
		}

		return
	} else if *statisticsDayToExport >= 0 {

		if *statisticsValidatorToggle {
			_, err := db.WriterDb.Exec("delete from validator_stats_status where day = $1", *statisticsDayToExport)
			if err != nil {
				logrus.Fatalf("error resetting status for day %v: %v", *statisticsDayToExport, err)
			}

			err = db.WriteValidatorStatisticsForDay(uint64(*statisticsDayToExport))
			if err != nil {
				logrus.Errorf("error exporting stats for day %v: %v", *statisticsDayToExport, err)
			}
		}

		if *statisticsChartToggle {
			_, err = db.WriterDb.Exec("delete from chart_series_status where day = $1", *statisticsDayToExport)
			if err != nil {
				logrus.Fatalf("error resetting status for chart series status for day %v: %v", *statisticsDayToExport, err)
			}

			err = db.WriteChartSeriesForDay(int64(*statisticsDayToExport))
			if err != nil {
				logrus.Errorf("error exporting chart series from day %v: %v", *statisticsDayToExport, err)
			}
		}
		return
	}

	go statisticsLoop()
	if !*poolsDisabledFlag {
		go poolsLoop()
	}

	utils.WaitForCtrlC()

	logrus.Println("exiting...")
}

func statisticsLoop() {
	for {

		latestEpoch, err := db.GetLatestEpoch()
		if err != nil {
			logrus.Errorf("error retreiving latest epoch from the db: %v", err)
			time.Sleep(time.Minute)
			continue
		}

		epochsPerDay := (24 * 60 * 60) / utils.Config.Chain.Config.SlotsPerEpoch / utils.Config.Chain.Config.SecondsPerSlot
		if latestEpoch < epochsPerDay {
			logrus.Infof("skipping exporting stats, first day has not been indexed yet")
			time.Sleep(time.Minute)
			continue
		}

		currentDay := latestEpoch / epochsPerDay
		previousDay := currentDay - 1

		if previousDay > currentDay {
			previousDay = currentDay
		}

		if opt.statisticsValidatorToggle {
			var lastExportedDayValidator uint64
			err = db.WriterDb.Get(&lastExportedDayValidator, "select COALESCE(max(day), 0) from validator_stats_status where status")
			if err != nil {
				logrus.Errorf("error retreiving latest exported day from the db: %v", err)
			}
			if lastExportedDayValidator != 0 {
				lastExportedDayValidator++
			}

			logrus.Infof("Validator Statistics: Latest epoch is %v, previous day is %v, last exported day is %v", latestEpoch, previousDay, lastExportedDayValidator)
			if lastExportedDayValidator <= previousDay || lastExportedDayValidator == 0 {
				for day := lastExportedDayValidator; day <= previousDay; day++ {
					err := db.WriteValidatorStatisticsForDay(day)
					if err != nil {
						logrus.Errorf("error exporting stats for day %v: %v", day, err)
					}
				}
			}

		}

		if opt.statisticsChartToggle {
			var lastExportedDayChart uint64
			err = db.WriterDb.Get(&lastExportedDayChart, "select COALESCE(max(day), 0) from chart_series_status where status")
			if err != nil {
				logrus.Errorf("error retreiving latest exported day from the db: %v", err)
			}
			if lastExportedDayChart != 0 {
				lastExportedDayChart++
			}
			logrus.Infof("Chart statistics: latest epoch is %v, previous day is %v, last exported day is %v", latestEpoch, previousDay, lastExportedDayChart)
			if lastExportedDayChart <= previousDay || lastExportedDayChart == 0 {
				for day := lastExportedDayChart; day <= previousDay; day++ {
					err = db.WriteChartSeriesForDay(int64(day))
					if err != nil {
						logrus.Errorf("error exporting chart series from day %v: %v", day, err)
					}
				}
			}
		}

		services.ReportStatus("statistics", "Running", nil)
		time.Sleep(time.Minute)
	}
}

func poolsLoop() {
	for {
		db.UpdatePoolInfo()
		services.ReportStatus("poolInfoUpdater", "Running", nil)
		time.Sleep(time.Minute * 10)
	}
}
