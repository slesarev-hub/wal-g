package greenplum

import (
	"fmt"
	"path"
	"strings"

	"github.com/greenplum-db/gp-common-go-libs/gplog"

	"github.com/greenplum-db/gp-common-go-libs/cluster"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/pkg/storages/storage"
)

type SegmentRestoreConfig struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	DataDir  string `json:"data_dir"`
}

// ClusterRestoreConfig is used to describe the restored cluster
type ClusterRestoreConfig struct {
	Segments map[int]SegmentRestoreConfig `json:"segments"`
}

type FetchHandler struct {
	cluster             *cluster.Cluster
	backupIDByContentID map[int]string
	backup              internal.Backup
}

func NewFetchHandler(backup internal.Backup, sentinel BackupSentinelDto, restoreCfg ClusterRestoreConfig) *FetchHandler {
	backupIDByContentID := make(map[int]string)
	segmentConfigs := make([]cluster.SegConfig, 0)
	gplog.InitializeLogging("wal-g", "")

	for _, segMeta := range sentinel.Segments {
		// currently, WAL-G does not restore the mirrors
		if segMeta.Role == Primary {
			// update the segment config from the metadata with the
			// Hostname, Port and DataDir specified in the restore config
			backupIDByContentID[segMeta.ContentID] = segMeta.BackupID
			segmentCfg := segMeta.ToSegConfig()
			segRestoreCfg, ok := restoreCfg.Segments[segMeta.ContentID]
			if !ok {
				tracelog.ErrorLogger.Fatalf(
					"Could not find content ID %d in the provided restore configuration", segMeta.ContentID)
			}
			segmentCfg.Hostname = segRestoreCfg.Hostname
			segmentCfg.Port = segRestoreCfg.Port
			segmentCfg.DataDir = segRestoreCfg.DataDir
			segmentConfigs = append(segmentConfigs, segmentCfg)
		} else {
			tracelog.WarningLogger.Printf(
				"Skipping non-primary segment: DatabaseID %d, Hostname %s, DataDir: %s\n", segMeta.DatabaseID, segMeta.Hostname, segMeta.DataDir)
		}
	}

	globalCluster := cluster.NewCluster(segmentConfigs)
	tracelog.DebugLogger.Printf("cluster %v\n", globalCluster)

	return &FetchHandler{
		cluster:             globalCluster,
		backupIDByContentID: backupIDByContentID,
		backup:              backup,
	}
}

func (fh *FetchHandler) Fetch() error {
	tracelog.InfoLogger.Println("Running wal-g on segments and master...")

	// Run WAL-G to restore the each segment as a single Postgres instance
	remoteOutput := fh.cluster.GenerateAndExecuteCommand("Running wal-g",
		cluster.ON_SEGMENTS|cluster.INCLUDE_MASTER,
		func(contentID int) string {
			return fh.buildFetchCommand(contentID)
		})

	fh.cluster.CheckClusterError(remoteOutput, "Unable to run wal-g", func(contentID int) string {
		return "Unable to run wal-g"
	})

	for _, command := range remoteOutput.Commands {
		tracelog.DebugLogger.Printf("WAL-G output (segment %d):\n%s\n", command.Content, command.Stderr)
	}

	tracelog.InfoLogger.Println("Updating pg_hba configs on segments...")
	err := fh.createPgHbaOnSegments()
	if err != nil {
		return err
	}
	tracelog.InfoLogger.Println("Creating recovery.conf files...")
	return fh.createRecoveryConfigs()
}

// createPgHbaOnSegments generates and uploads the correct pg_hba.conf
// files to each segment instance (except the master) so they can communicate correctly
func (fh *FetchHandler) createPgHbaOnSegments() error {
	pgHbaMaker := NewPgHbaMaker(fh.cluster.ByContent)
	fileContents, err := pgHbaMaker.Make()
	if err != nil {
		return err
	}

	remoteOutput := fh.cluster.GenerateAndExecuteCommand("Updating pg_hba on segments",
		cluster.ON_SEGMENTS|cluster.EXCLUDE_MIRRORS,
		func(contentID int) string {
			segment := fh.cluster.ByContent[contentID][0]
			pathToHba := path.Join(segment.DataDir, "pg_hba.conf")

			cmd := fmt.Sprintf("cat > %s << EOF\n%s\nEOF", pathToHba, fileContents)
			tracelog.DebugLogger.Printf("Command to run on segment %d: %s", contentID, cmd)
			return cmd
		})

	fh.cluster.CheckClusterError(remoteOutput, "Unable to update pg_hba", func(contentID int) string {
		return fmt.Sprintf("Unable to update pg_hba on segment %d", contentID)
	})

	for _, command := range remoteOutput.Commands {
		tracelog.DebugLogger.Printf("Update pg_hba output (segment %d):\n%s\n", command.Content, command.Stderr)
	}
	return nil
}

// createRecoveryConfigs generates and uploads the correct recovery.conf
// files to each segment instance (including master) so they can recover correctly
// during the database startup
func (fh *FetchHandler) createRecoveryConfigs() error {
	restoreCfgMaker := NewRecoveryConfigMaker("/usr/bin/wal-g", internal.CfgFile, fh.backup.Name)

	remoteOutput := fh.cluster.GenerateAndExecuteCommand("Creating recovery.conf on segments and master",
		cluster.ON_SEGMENTS|cluster.EXCLUDE_MIRRORS|cluster.INCLUDE_MASTER,
		func(contentID int) string {
			segment := fh.cluster.ByContent[contentID][0]
			pathToRestore := path.Join(segment.DataDir, "recovery.conf")
			fileContents := restoreCfgMaker.Make(contentID)
			cmd := fmt.Sprintf("cat > %s << EOF\n%s\nEOF", pathToRestore, fileContents)
			tracelog.DebugLogger.Printf("Command to run on segment %d: %s", contentID, cmd)
			return cmd
		})

	fh.cluster.CheckClusterError(remoteOutput, "Unable to create recovery.conf", func(contentID int) string {
		return fmt.Sprintf("Unable to create recovery.conf on segment %d", contentID)
	})

	for _, command := range remoteOutput.Commands {
		tracelog.DebugLogger.Printf("Create recovery.conf output (segment %d):\n%s\n", command.Content, command.Stderr)
	}
	return nil
}

// buildFetchCommand creates the WAL-G command to restore the segment with
// the provided contentID
func (fh *FetchHandler) buildFetchCommand(contentID int) string {
	segment := fh.cluster.ByContent[contentID][0]
	backupID, ok := fh.backupIDByContentID[contentID]
	if !ok {
		// this should never happen
		tracelog.ErrorLogger.Fatalf("Failed to load backup id by content id %d", contentID)
	}

	segUserData := NewSegmentUserDataFromID(backupID)
	cmd := []string{
		"WALG_LOG_LEVEL=DEVEL",
		fmt.Sprintf("PGPORT=%d", segment.Port),
		"wal-g pg",
		fmt.Sprintf("backup-fetch %s", segment.DataDir),
		fmt.Sprintf("--walg-storage-prefix=%d", segment.ContentID),
		fmt.Sprintf("--target-user-data=%s", segUserData.QuotedString()),
		fmt.Sprintf("--config=%s", internal.CfgFile),
	}

	cmdLine := strings.Join(cmd, " ")
	tracelog.DebugLogger.Printf("Command to run on segment %d: %s", contentID, cmdLine)
	return cmdLine
}

func NewGreenplumBackupFetcher(restoreCfg ClusterRestoreConfig) func(folder storage.Folder, backup internal.Backup) {
	return func(folder storage.Folder, backup internal.Backup) {
		var sentinel BackupSentinelDto
		err := backup.FetchSentinel(&sentinel)
		tracelog.ErrorLogger.FatalOnError(err)

		err = NewFetchHandler(backup, sentinel, restoreCfg).Fetch()
		tracelog.ErrorLogger.FatalOnError(err)
	}
}
