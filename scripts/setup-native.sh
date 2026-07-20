#!/usr/bin/env bash
# Build and install the native libraries hap needs since the semantic
# signature matcher landed:
#
#   - libbinding.a + static llama.cpp archives from submodule/github.com/seed-hypermedia/llama-go
#     (llama.cpp embeddings; BUILD_SHARED_LIBS=OFF so the hap binary links
#     them statically — no llama/ggml runtime libraries to ship)
#   - libfaiss_c + libfaiss shared libs from the blevesearch FAISS fork
#     (bleve vector search behind the `vectors` build tag)
#
# After this script succeeds, build and test with:
#   go build -tags "vectors cpu" ./...
#   go test  -tags "vectors cpu" ./... -count=1
#
# Idempotent: finished artifacts are skipped, so re-runs are cheap. Set
# HAP_NATIVE_CACHE to relocate the FAISS checkout (default .cache/native).
set -euo pipefail

cd "$(dirname "$0")/.."
REPO_ROOT="$(pwd)"

# blevesearch/faiss checkpoint paired with bleve v2.6.0 (docs/vectors.md).
FAISS_REPO="https://github.com/blevesearch/faiss.git"
FAISS_COMMIT="fff814dea2bdda020363506904979b204ee201aa"
CACHE="${HAP_NATIVE_CACHE:-${REPO_ROOT}/.cache/native}"
PREFIX="${HAP_NATIVE_PREFIX:-/usr/local}"
JOBS="${HAP_NATIVE_JOBS:-$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 2)}"

SUDO="sudo"
if [ -w "${PREFIX}/lib" ] || [ "$(id -u)" = "0" ]; then SUDO=""; fi

OS="$(uname -s)"
case "$OS" in
  Darwin) SOEXT="dylib" ;;
  *) SOEXT="so" ;;
esac

# FAISS needs BLAS and OpenMP. Linux: OpenBLAS + gcc's libgomp. macOS: the
# Accelerate framework provides BLAS, but Apple clang has no OpenMP — bleve's
# documented recipe builds FAISS with Homebrew LLVM (which bundles libomp).
FAISS_CMAKE_ENV=()
if [ "$OS" = "Linux" ]; then
  if ! ldconfig -p 2>/dev/null | grep -q libopenblas && command -v apt-get >/dev/null 2>&1; then
    echo "==> installing libopenblas-dev (FAISS BLAS backend)"
    $SUDO apt-get update -qq && $SUDO apt-get install -y -qq libopenblas-dev
  fi
  # Both builds below shell out to cmake; without this the script dies ~100
  # lines later with a bare "cmake: command not found" and no hint that a
  # prerequisite is missing.
  if ! command -v cmake >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
    echo "==> installing cmake (llama.cpp + FAISS build driver)"
    $SUDO apt-get update -qq && $SUDO apt-get install -y -qq cmake
  fi
elif [ "$OS" = "Darwin" ]; then
  command -v brew >/dev/null 2>&1 || { echo "Homebrew is required on macOS (for LLVM/OpenMP)" >&2; exit 1; }
  if ! brew list llvm >/dev/null 2>&1; then
    echo "==> installing Homebrew LLVM (OpenMP toolchain for FAISS)"
    brew install llvm
  fi
  LLVM_PREFIX="$(brew --prefix llvm)"
  FAISS_CMAKE_ENV=(
    "CC=${LLVM_PREFIX}/bin/clang"
    "CXX=${LLVM_PREFIX}/bin/clang++"
    "LDFLAGS=-L${LLVM_PREFIX}/lib"
    "CPPFLAGS=-I${LLVM_PREFIX}/include"
  )
fi

# Anything the auto-install above could not cover (no apt-get, or macOS).
command -v cmake >/dev/null 2>&1 || {
  echo "cmake is required to build llama.cpp and FAISS — install it and re-run" >&2
  echo "  Debian/Ubuntu: sudo apt-get install -y cmake" >&2
  echo "  macOS:         brew install cmake" >&2
  exit 1
}

echo "==> submodules (llama-go + shallow llama.cpp)"
git submodule update --init submodule/github.com/seed-hypermedia/llama-go
git -C submodule/github.com/seed-hypermedia/llama-go submodule update --init --depth 1 llama.cpp

echo "==> llama-go static binding (CPU only, static archives)"
# BUILD_SHARED_LIBS=OFF makes the Makefile copy .a archives — the shared
# branch hardcodes .so names and breaks on macOS. GPU/BLAS backends are off:
# hap embeds short strings on CPU and links with the `cpu` build tag.
# x86 ONLY: GGML_NATIVE=OFF stops ggml passing -march=native, which would bake
# the BUILDER's CPU features into the archive — a release runner with AVX-512
# then produces an amd64 binary that SIGILLs on load on AVX2-only hosts. We pin
# an explicit AVX2 baseline (x86-64-v3, every 2013+ Haswell) so portability
# isn't scalar-slow. arm64 is deliberately left on ggml's default tuning: its
# build runs on Apple Silicon / arm servers that are forward-compatible, and
# scoping the change to x86 keeps it clear of the arm64 embedder abort tracked
# in #60. CGO builds each platform on its own native runner, so `uname -m` is
# the target arch.
LLAMA_CMAKE_ARGS="-DBUILD_SHARED_LIBS=OFF -DGGML_METAL=OFF -DGGML_BLAS=OFF -DGGML_ACCELERATE=OFF"
LLAMA_NATIVE_OFF=0
case "$(uname -m)" in
  x86_64 | amd64)
    LLAMA_CMAKE_ARGS="${LLAMA_CMAKE_ARGS} -DGGML_NATIVE=OFF -DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON -DGGML_AVX512=OFF"
    LLAMA_NATIVE_OFF=1
    ;;
esac
# The llama-go Makefile only invalidates its cmake cache on GPU-flag mismatches,
# not shared/static or the native→baseline switch — wipe when the cached config
# disagrees with what we want now: a build configured for shared libs, or whose
# cached GGML_NATIVE setting differs from this arch's target (x86 wants it OFF;
# arm64 wants ggml's default, so a leftover x86/global OFF must be re-configured).
LLAMA_CACHE=submodule/github.com/seed-hypermedia/llama-go/build/CMakeCache.txt
llama_cache_stale=0
if [ -f "$LLAMA_CACHE" ]; then
  grep -q "BUILD_SHARED_LIBS:BOOL=OFF" "$LLAMA_CACHE" || llama_cache_stale=1
  cache_native_off=0
  if grep -q "GGML_NATIVE:BOOL=OFF" "$LLAMA_CACHE"; then cache_native_off=1; fi
  [ "$cache_native_off" = "$LLAMA_NATIVE_OFF" ] || llama_cache_stale=1
fi
if [ "$llama_cache_stale" = 1 ]; then
  echo "    (wiping stale cmake cache: shared-lib or native config)"
  rm -rf submodule/github.com/seed-hypermedia/llama-go/build submodule/github.com/seed-hypermedia/llama-go/llama.cpp/*.o \
    submodule/github.com/seed-hypermedia/llama-go/*.o submodule/github.com/seed-hypermedia/llama-go/*.a \
    submodule/github.com/seed-hypermedia/llama-go/*.so submodule/github.com/seed-hypermedia/llama-go/*.dylib
fi
if [ ! -f submodule/github.com/seed-hypermedia/llama-go/libbinding.a ] || [ ! -f submodule/github.com/seed-hypermedia/llama-go/libllama.a ]; then
  rm -f submodule/github.com/seed-hypermedia/llama-go/*.a submodule/github.com/seed-hypermedia/llama-go/*.so submodule/github.com/seed-hypermedia/llama-go/*.dylib
  CMAKE_ARGS="${LLAMA_CMAKE_ARGS}" make -C submodule/github.com/seed-hypermedia/llama-go libbinding.a -j"${JOBS}"
fi

echo "==> FAISS (blevesearch fork @ ${FAISS_COMMIT:0:7})"
mkdir -p "${CACHE}"
if [ ! -d "${CACHE}/faiss" ]; then
  git clone --quiet "${FAISS_REPO}" "${CACHE}/faiss"
fi
git -C "${CACHE}/faiss" checkout --quiet "${FAISS_COMMIT}"
if [ ! -f "${CACHE}/faiss/build/c_api/libfaiss_c.${SOEXT}" ]; then
  env ${FAISS_CMAKE_ENV[@]+"${FAISS_CMAKE_ENV[@]}"} cmake -S "${CACHE}/faiss" -B "${CACHE}/faiss/build" \
    -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=ON \
    -DFAISS_ENABLE_GPU=OFF \
    -DFAISS_ENABLE_PYTHON=OFF \
    -DFAISS_ENABLE_C_API=ON \
    -DFAISS_OPT_LEVEL=generic \
    -DBUILD_TESTING=OFF \
    -DCMAKE_INSTALL_RPATH='$ORIGIN' \
    -DCMAKE_CXX_FLAGS="-I${CACHE}/faiss"
  make -C "${CACHE}/faiss/build" -j"${JOBS}" faiss
  make -C "${CACHE}/faiss/build" faiss_c
fi

echo "==> install FAISS shared libraries to ${PREFIX}/lib"
$SUDO mkdir -p "${PREFIX}/lib" "${PREFIX}/include/faiss"
$SUDO cp "${CACHE}/faiss/build/faiss/libfaiss.${SOEXT}" \
  "${CACHE}/faiss/build/c_api/libfaiss_c.${SOEXT}" "${PREFIX}/lib/"
# go-faiss includes <faiss/c_api/...> headers at build time.
$SUDO cp -r "${CACHE}/faiss/c_api" "${PREFIX}/include/faiss/"
if command -v ldconfig >/dev/null 2>&1; then $SUDO ldconfig; fi

echo "==> done. Build with: go build -tags \"vectors cpu\" ./..."
