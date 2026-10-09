#!/usr/bin/env bash
# DIAG ONLY (never merged): interleave single-process and --linger OOM trials.
# usage: loop.sh <runtime> <image> <iterations> <outdir>
set -uo pipefail
rt="$1" image="$2" n="$3" out="$4"
here="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$out"
scratch=$(mktemp -d)
: >"$scratch/release" && chmod 0644 "$scratch/release"
tar -cf "$scratch/release.tar" -C "$scratch" release
for ((i=0; i<n; i++)); do
    if (( i % 2 == 0 )); then order=(single linger); else order=(linger single); fi
    for mode in "${order[@]}"; do
        bash "$here/trial.sh" "$rt" "$image" "$mode" "$i" "$out" "$scratch/release.tar" | tee -a "$out/results"
    done
done
