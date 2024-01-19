#!/usr/bin/env bash

#
# Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#

set -ex

function zkConfig() {
  if [ -n "$1" ]; then
    FQDN="$1"
  else
    FQDN="$HOST.$DOMAIN"
  fi
  echo "$FQDN:$QUORUM_PORT:$LEADER_PORT:$ROLE;$CLIENT_PORT"
}

function zkConnectionString() {
  # If the client service address is not yet available, then return localhost
  set +e
  getent hosts "${CLIENT_HOST}" 2>/dev/null 1>/dev/null
  if [[ $? -ne 0 ]]; then
    set -e
    echo "localhost:${CLIENT_PORT}"
  else
    set -e
    echo "${CLIENT_HOST}:${CLIENT_PORT}"
  fi
}

# TODO: add a function for getting extra addresses
# need to raise an exception if role isn't set but an address is found
function myExtraAddress() {
  EXTRAADDRESSFILE=/conf/addServerAddresses.txt
  if [ -f $EXTRAADDRESSFILE ]; then
    prefix="server.${MYID}="
    while IFS= read -r line; do
      if [[ "$line"  == "$prefix"* ]]; then
        EXTRAADDRESS=${line#"$prefix"}
        EXTRACONFIG=$(zkConfig $EXTRAADDRESS)
      fi
    done < $EXTRAADDRESSFILE
  fi
  
  if [ -n "$EXTRACONFIG" ]; then
    echo "$EXTRACONFIG"
  fi
}