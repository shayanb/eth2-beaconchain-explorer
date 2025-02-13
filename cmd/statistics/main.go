package main

import (
	"eth2-exporter/db"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"flag"
	"time"

	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/sirupsen/logrus"
)

func main() {
	configPath := flag.String("config", "", "Path to the config file")
	statisticsDayToExport := flag.Int64("statistics.day", -1, "Day to export statistics (will export the day independent if it has been already exported or not")
	streaksDisabledFlag := flag.Bool("streaks.disabled", false, "Disable exporting streaks")
	poolsDisabledFlag := flag.Bool("pools.disabled", false, "Disable exporting pools")

	flag.Parse()

	logrus.Printf("config file path: %v", *configPath)
	cfg := &types.Config{}
	err := utils.ReadConfig(cfg, *configPath)

	if err != nil {
		logrus.Fatalf("error reading config file: %v", err)
	}
	utils.Config = cfg

	db.MustInitDB(cfg.Database.Username, cfg.Database.Password, cfg.Database.Host, cfg.Database.Port, cfg.Database.Name)
	defer db.DB.Close()

	if *statisticsDayToExport >= 0 {
		_, err := db.DB.Exec("delete from validator_stats_status where day = $1", *statisticsDayToExport)
		if err != nil {
			logrus.Fatalf("error resetting status for day %v: %v", *statisticsDayToExport, err)
		}

		err = db.WriteStatisticsForDay(uint64(*statisticsDayToExport))
		if err != nil {
			logrus.Errorf("error exporting stats for day %v: %v", *statisticsDayToExport, err)
		}
		return
	}

	go statisticsLoop()
	if !*streaksDisabledFlag {
		go streaksLoop()
	}
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

		epochsPerDay := (24 * 60 * 60) / utils.Config.Chain.SlotsPerEpoch / utils.Config.Chain.SecondsPerSlot
		if latestEpoch < epochsPerDay {
			logrus.Infof("skipping exporting validator_stats, first day has not been indexed yet")
			time.Sleep(time.Minute)
			continue
		}

		currentDay := latestEpoch / epochsPerDay
		previousDay := currentDay - 1

		if previousDay > currentDay {
			previousDay = currentDay
		}

		var lastExportedDay uint64
		err = db.DB.Get(&lastExportedDay, "select COALESCE(max(day), 0) from validator_stats_status where status")
		if err != nil {
			logrus.Errorf("error retreiving latest exported day from the db: %v", err)
		}
		if lastExportedDay != 0 {
			lastExportedDay++
		}

		logrus.Infof("latest epoch is %v, previous day is %v, last exported day is %v", latestEpoch, previousDay, lastExportedDay)
		if lastExportedDay <= previousDay || lastExportedDay == 0 {
			for day := lastExportedDay; day <= previousDay; day++ {
				err := db.WriteStatisticsForDay(day)
				if err != nil {
					logrus.Errorf("error exporting stats for day %v: %v", day, err)
				}
			}
		}
		time.Sleep(time.Minute)
	}
}

func streaksLoop() {
	for {
		done, err := db.UpdateAttestationStreaks()
		if err != nil {
			logrus.WithError(err).Error("error updating attesation_streaks")
		}
		if done {
			// updated streaks up to the current finalized epoch
			time.Sleep(time.Second * 3600)
		} else {
			// go faster until streaks are upated to the current finalized epoch
			time.Sleep(time.Second * 10)
		}
	}
}

func poolsLoop() {
	for {
		db.UpdatePoolInfo()
		time.Sleep(time.Minute * 10)
	}
}
