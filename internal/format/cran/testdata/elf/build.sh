#!/bin/bash
# Rebuilds the ELF fixtures for the CRAN Linux binary validation tests
# (linuxbin_test.go). Run from anywhere; needs docker. The outputs are
# committed, so this only runs when a fixture has to change.
#
#   x86_64-plain.so     EM_X86_64,  NEEDED libc.so.6,             GLIBC_2.2.5 / 2.14
#   x86_64-glibc238.so  EM_X86_64,  NEEDED libc.so.6,             needs GLIBC_2.38 (strlcpy)
#   x86_64-libfoo.so    EM_X86_64,  NEEDED libfoo.so.1
#   aarch64-plain.so    EM_AARCH64, NEEDED libc.so.6
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
docker run --rm -v "$here":/out -w /tmp ubuntu:24.04 bash -euc '
apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends gcc libc6-dev gcc-aarch64-linux-gnu libc6-dev-arm64-cross >/dev/null
cat > plain.c <<C
#include <string.h>
void fx_copy(char *d, const char *s, size_t n) { memcpy(d, s, n); }
C
cat > g238.c <<C
#include <string.h>
size_t fx_lcpy(char *d, const char *s, size_t n) { return strlcpy(d, s, n); }
C
cat > foo.c <<C
int foo(void) { return 1; }
C
cat > usefoo.c <<C
int foo(void);
int fx_usefoo(void) { return foo(); }
C
flags="-shared -fPIC -O2 -s -nostartfiles"
gcc $flags -o /out/x86_64-plain.so plain.c
gcc $flags -o /out/x86_64-glibc238.so g238.c
gcc $flags -Wl,-soname,libfoo.so.1 -o libfoo.so.1 foo.c && ln -sf libfoo.so.1 libfoo.so
gcc $flags -o /out/x86_64-libfoo.so usefoo.c -L. -lfoo
aarch64-linux-gnu-gcc $flags -Wl,-z,max-page-size=4096 -o /out/aarch64-plain.so plain.c
chown '"$(id -u):$(id -g)"' /out/*.so
'
ls -l "$here"/*.so
