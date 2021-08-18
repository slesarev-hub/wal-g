package functests

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/DATA-DOG/godog"
	"github.com/DATA-DOG/godog/gherkin"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/tests_func/helpers"
	"github.com/wal-g/wal-g/tests_func/utils"
)

const (
	featuresDir = "features"
	featureExt  = ".feature"

	MAX_RETRIES_COUNT = 10
)

func (tctx *TestContext) ContainerFQDN(name string) string {
	return fmt.Sprintf("%s.test_net_%s", name, tctx.Env["TEST_ID"])
}

func (tctx *TestContext) S3Host() string {
	return tctx.Env["S3_HOST"]
}

func WalgUtilFromTestContext(tctx *TestContext, host string) *helpers.WalgUtil {
	return helpers.NewWalgUtil(
		tctx.Context,
		tctx.ContainerFQDN(host),
		tctx.Env["WALG_CLIENT_PATH"],
		tctx.Env["WALG_CONF_PATH"],
		tctx.Version.Major)
}

func S3StorageFromTestContext(tctx *TestContext, host string) *helpers.S3Storage {
	return helpers.NewS3Storage(
		tctx.Context,
		tctx.ContainerFQDN(host),
		tctx.Env["S3_BUCKET"],
		tctx.Env["S3_ACCESS_KEY"],
		tctx.Env["S3_SECRET_KEY"])
}

func MongoCtlFromTestContext(tctx *TestContext, host string) (*helpers.MongoCtl, error) {
	return helpers.NewMongoCtl(
		tctx.Context,
		tctx.ContainerFQDN(host),
		helpers.AdminCreds(helpers.AdminCredsFromEnv(tctx.Env)))
}

func InfraFromTestContext(tctx *TestContext) *helpers.Infra {
	return helpers.NewInfra(
		tctx.Context,
		tctx.Env["COMPOSE_FILE"],
		tctx.Env,
		tctx.Env["NETWORK_NAME"],
		helpers.BaseImage{Path: tctx.Env["BACKUP_BASE_PATH"], Tag: tctx.Env["BACKUP_BASE_TAG"]})
}

type AuxData struct {
	Timestamps         map[string]helpers.OpTimestamp
	Snapshots          map[string][]helpers.NsSnapshot
	CreatedBackupNames []string
	NometaBackupNames  []string
	OplogPushEnabled   bool
}

type MongoVersion struct {
	Major string
	Full  string
}

type TestContext struct {
	EnvFilePath        string
	Database           string
	Infra              *helpers.Infra
	Env                map[string]string
	Context            context.Context
	AuxData            AuxData
	Version            MongoVersion
	Features           []string
	PreviousBackupTime time.Time
}

func NewTestContext(envFilePath, database string, env, features map[string]string) (*TestContext, error) {
	featuresList := utils.GetMapValues(features)
	environ := utils.ParseEnvLines(os.Environ())
	return &TestContext{
		EnvFilePath: envFilePath,
		Database:    database,
		Context:     context.Background(),
		Version: MongoVersion{
			Major: environ["MONGO_MAJOR"],
			Full:  environ["MONGO_VERSION"]},
		Features: featuresList,
		Env:      env}, nil
}

func (tctx *TestContext) StopEnv() error {
	return tctx.Infra.Shutdown()
}

func (tctx *TestContext) CleanEnv() error {
	// TODO: Enable net cleanup
	//if err := helpers.RemoveNet(TestContext); err != nil {
	//	log.Fatalln(err)
	//}

	envFilePath := tctx.EnvFilePath
	stagingPath := path.Dir(envFilePath)
	return os.RemoveAll(stagingPath)
}

func (tctx *TestContext) setupSuites(s *godog.Suite) {
	s.BeforeFeature(func(feature *gherkin.Feature) {
		tctx.AuxData.CreatedBackupNames = []string{}
		tctx.AuxData.NometaBackupNames = []string{}
		tctx.AuxData.OplogPushEnabled = false
		tctx.AuxData.Timestamps = make(map[string]helpers.OpTimestamp)
		tctx.AuxData.Snapshots = make(map[string][]helpers.NsSnapshot)
		tctx.PreviousBackupTime = time.Unix(0, 0)
		if err := tctx.Infra.RecreateContainers(); err != nil {
			tracelog.ErrorLogger.Fatalln(err)
		}
	})

	s.BeforeSuite(tctx.LoadEnv)

	s.Step(`^a configured s3 on ([^\s]*)$`, tctx.configureS3)

	s.Step(`^a working mongodb on ([^\s]*)$`, tctx.testMongoConnect)
	s.Step(`^mongodb replset initialized on ([^\s]*)$`, tctx.initiateReplSet)
	s.Step(`^mongodb role is primary on ([^\s]*)$`, tctx.isMongoPrimary)
	s.Step(`^mongodb auth initialized on ([^\s]*)$`, tctx.mongoEnableAuth)
	s.Step(`^mongodb initialized on ([^\s]*)$`, tctx.mongoInit)
	s.Step(`^([^\s]*) has no data$`, tctx.purgeMongoDataDir)

	s.Step(`we save last oplog timestamp on ([^\s]*) to "([^"]*)"`, tctx.saveOplogTimestamp)
	s.Step(`^([^\s]*) has test mongodb data test(\d+)$`, tctx.fillMongodbWithTestData)
	s.Step(`^([^\s]*) has been loaded with "([^"]*)"$`, tctx.loadMongodbOpsFromConfig)
	s.Step(`^we got same mongodb data at ([^\s]*) ([^\s]*)$`, tctx.testEqualMongodbDataAtHosts)
	s.Step(`^we have same data in "([^"]*)" and "([^"]*)"$`, tctx.sameDataCheck)
	s.Step(`^we save ([^\s]*) data "([^"]*)"$`, tctx.saveMongoSnapshot)

	s.Step(`^we create ([^\s]*) mongo-backup$`, tctx.createMongoBackup)
	s.Step(`^we delete mongo backups retain (\d+) via ([^\s]*)$`, tctx.purgeBackupRetain)
	s.Step(`^at least one oplog archive exists in storage$`, tctx.oplogArchiveIsNotEmpty)
	s.Step(`^we purge oplog archives via ([^\s]*)$`, tctx.purgeOplogArchives)
	s.Step(`^we restore #(\d+) backup to ([^\s]*)$`, tctx.restoreBackupToMongodb)
	s.Step(`^oplog archiving is enabled on ([^\s]*)$`, tctx.enableOplogPush)
	s.Step(`^we restore from #(\d+) backup to "([^"]*)" timestamp to ([^\s]*)$`, tctx.replayOplog)

	s.Step(`^we got (\d+) backup entries of ([^\s]*)$`, tctx.checkBackupsCount)
	s.Step(`^we delete mongo backup #(\d+) via ([^\s]*)$`, tctx.deleteMongoBackup)
	s.Step(`^we ensure ([^\s]*) #(\d+) backup metadata contains$`, tctx.backupMetadataContains)
	s.Step(`^we put empty backup via ([^\s]*) to ([^\s]*)$`, tctx.putEmptyBackupViaMinio)
	s.Step(`^we check if empty backups were purged via ([^\s]*)$`, tctx.testEmptyBackupsViaMinio)

	s.Step(`we sleep ([^\s]*)$`, tctx.sleep)

	s.Step(`^a working redis on ([^\s]*)$`, tctx.isWorkingRedis)
	s.Step(`^([^\s]*) has test redis data test(\d+)$`, tctx.redisHasTestRedisDataTest)
	s.Step(`^we create ([^\s]*) redis-backup$`, tctx.createRedisBackup)
	s.Step(`^we delete redis backups retain (\d+) via ([^\s]*)$`, tctx.weDeleteRedisBackupsRetainViaRedis)
	s.Step(`^we restart redis-server at ([^\s]*)$`, tctx.weRestartRedisServerAt)
	s.Step(`^we got same redis data at ([^\s]*) ([^\s]*)$`, tctx.testEqualRedisDataAtHosts)
}

func (tctx *TestContext) LoadEnv() {
	env := tctx.Env
	var err error
	if env == nil {
		env, err = ReadEnv(tctx.EnvFilePath)
		tracelog.ErrorLogger.FatalOnError(err)
	}

	// mix os.environ to our database params
	tctx.Env = utils.MergeEnvs(utils.ParseEnvLines(os.Environ()), env)

	tctx.Infra = InfraFromTestContext(tctx)
	err = tctx.Infra.Setup()
	tracelog.ErrorLogger.FatalOnError(err)
}

func scanFeatureDirs(dbName, featurePrefix string) (map[string]string, error) {
	dir := path.Join(featuresDir, dbName)
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	environ := utils.ParseEnvLines(os.Environ())
	requestedFeature := environ["FEATURE"]
	if requestedFeature != "" {
		found := false
		for _, f := range files {
			filename := f.Name()
			if filename == requestedFeature+featureExt {
				files = []os.FileInfo{f}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("requested feature is not found: %s", requestedFeature)
		}
	}

	foundFeatures := make(map[string]string)
	for _, f := range files {
		filename := f.Name()

		if featurePrefix != "" && !strings.HasPrefix(filename, featurePrefix) {
			continue // skip feature
		}

		if strings.HasSuffix(filename, featureExt) {
			featureName := filename[0 : len(filename)-len(featureExt)]
			foundFeatures[featureName] = path.Join(dir, f.Name())
		}
	}

	return foundFeatures, nil
}

func GetRedisCtlFromTestContext(tctx *TestContext, hostName string) (*helpers.RedisCtl, error) {
	host := tctx.ContainerFQDN(hostName)
	port, err := strconv.Atoi(tctx.Env["REDIS_EXPOSE_PORT"])
	if err != nil {
		return nil, err
	}
	return helpers.NewRedisCtl(
		tctx.Context,
		host,
		port,
		tctx.Env["REDIS_PASSWORD"],
		tctx.Env["WALG_CLIENT_PATH"],
		tctx.Env["WALG_CONF_PATH"],
	)
}
