#!/bin/bash
set -e -x

CONFIG_FILE="/tmp/configs/full_backup_test_config.json"
COMMON_CONFIG="/tmp/configs/common_config.json"
TMP_CONFIG="/tmp/configs/tmp_config.json"
cat ${CONFIG_FILE} > ${TMP_CONFIG}
echo "," >> ${TMP_CONFIG}
cat ${COMMON_CONFIG} >> ${TMP_CONFIG}
/tmp/pg_scripts/wrap_config_file.sh ${TMP_CONFIG}

/home/gpadmin/run_greenplum.sh

source /usr/local/gpdb_src/gpAux/gpdemo/gpdemo-env.sh && /usr/local/gpdb_src/bin/createdb
sleep 10

wal-g backup-push --config=${TMP_CONFIG}

echo "Greenplum backup-push test was successful"