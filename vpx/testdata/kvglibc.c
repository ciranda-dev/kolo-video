#include <pthread.h>

/* Needs GLIBC_2.34 (pthread_create moved into libc.so.6), so a host with an older glibc refuses to load it. */
static void *run(void *p) { return p; }

int kv_abi_version(void) {
	pthread_t t;
	if (pthread_create(&t, 0, run, 0) == 0) pthread_join(t, 0);
	return 1;
}
