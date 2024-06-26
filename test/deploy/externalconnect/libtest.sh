#!/bin/bash

# Copyright (C) 2024  mieru authors
#
# This program is free software: you can redistribute it and/or modify
# it under the terms of the GNU General Public License as published by
# the Free Software Foundation, either version 3 of the License, or
# (at your option) any later version.
#
# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program.  If not, see <https://www.gnu.org/licenses/>.

function run_external_connect_test() {
    success_count=0
    failure_count=0

    for i in {1..1000}; do
        ./mieru test
        if [[ "$?" -ne 0 ]]; then
            ((failure_count++))
            echo "Failed $failure_count times with $i runs."
            if [[ "$failure_count" -ge 2 ]]; then
                echo "Test failed: too many runs have a non-zero exit code."
                return 1
            fi
        else
            ((success_count++))
        fi
        sleep 1
    done

    return 0
}

function print_mieru_client_log() {
    echo "========== BEGIN OF MIERU CLIENT LOG =========="
    cat $HOME/.cache/mieru/*.log
    echo "==========  END OF MIERU CLIENT LOG  =========="
}

function delete_mieru_client_log() {
    rm -rf $HOME/.cache/mieru/*.log
}

function print_mieru_client_metrics() {
    echo "========== BEGIN OF MIERU CLIENT METRICS =========="
    ./mieru get metrics
    ./mieru get memory-statistics
    echo "==========  END OF MIERU CLIENT METRICS  =========="
}

function print_mieru_server_metrics() {
    echo "========== BEGIN OF MIERU SERVER METRICS =========="
    ./mita get metrics
    ./mita get memory-statistics
    echo "==========  END OF MIERU SERVER METRICS  =========="
}
