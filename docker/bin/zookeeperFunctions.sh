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

function myExtraAddress() {
  # NOTE: must have ROLE defined test gh access
  EXTRAADDRESSFILE=/conf/addServerAddresses.txt
  if [ -f $EXTRAADDRESSFILE ]; then
    prefix="server.${MYID}="
    while IFS= read -r line; do
      if [[ "$line"  == "$prefix"* ]]; then
        EXTRAADDRESS=${line#"$prefix"}
        # if extra address has a '|' in it (multiple addresses included)
        if  [[ "$EXTRAADDRESS" == *"|"* ]]; then
          # split at pipe
          suffix=";${CLIENT_PORT}"
          IFS="|"
          read -ra addresses <<< "$EXTRAADDRESS"
          # add quorum and leader ports
          EXTRACONFIG=""
          for address in "${addresses[@]}"; do
            address_config=$(zkConfig $address)
            EXTRACONFIG+="${EXTRACONFIG}${address_config%$suffix}|"
          done
          # remove last pipe and add the clientport back in
          pipe="|"
          EXTRACONFIG="${EXTRACONFIG%$pipe}${suffix}"
          IFS=
        else
          EXTRACONFIG=$(zkConfig $EXTRAADDRESS)
        fi
      fi
    done < $EXTRAADDRESSFILE
  fi

  if [ -n "$EXTRACONFIG" ]; then
    echo "$EXTRACONFIG"
  fi
}