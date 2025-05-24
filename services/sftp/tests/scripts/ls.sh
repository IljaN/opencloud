#!/bin/bash

sshpass -p '1' sftp -P 2222 admin@127.0.0.1 <<EOF
pwd
ls -la
ls Admin
cd Admin
pwd
ls -la
ls FolderBaz
bye
EOF
