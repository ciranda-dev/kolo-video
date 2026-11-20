/* Exports the kv_* interface of the stub library with ABI version 2, so the loader
   of vpx.Load must refuse it and fall back. It contains no codec. */
#include <stdint.h>

#ifdef _WIN32
#define X __declspec(dllexport)
#else
#define X __attribute__((visibility("default")))
#endif

static uintptr_t mix(uintptr_t h, uintptr_t v) { return h * 31 + v; }

X int kv_abi_version(void) { return 2; }
X const char *kv_version(void) { return "v0.0.0-stub"; }

X uintptr_t kv_enc_new(int vp9, int w, int h, unsigned kbps, int threads, int speed, unsigned max_intra, int *err) {
	*err = 7;
	uintptr_t r = (uintptr_t)vp9;
	r = mix(r, (uintptr_t)(intptr_t)w);
	r = mix(r, (uintptr_t)(intptr_t)h);
	r = mix(r, kbps);
	r = mix(r, (uintptr_t)(intptr_t)threads);
	r = mix(r, (uintptr_t)(intptr_t)speed);
	return mix(r, max_intra);
}

X void kv_enc_free(uintptr_t e) { (void)e; }

X int kv_encode(uintptr_t e, const uint8_t *y, int ys, const uint8_t *u, const uint8_t *v, int uvs, int64_t pts, uint32_t dur, int key) {
	(void)e;
	return (int)(y[0] + 3 * u[0] + 5 * v[0] + ys + 7 * uvs + (int)pts + (int)(dur * 11) + key * 13);
}

X int kv_next_frame(uintptr_t e, uintptr_t *out) {
	out[0] = e;
	out[1] = 42;
	out[2] = 1;
	return 1;
}

X int kv_set_bitrate(uintptr_t e, unsigned kbps) { return (int)(e + kbps); }

X uintptr_t kv_dec_new(int vp9, int threads, int *err) {
	*err = 9;
	return mix(mix((uintptr_t)vp9, (uintptr_t)(intptr_t)threads), 5);
}

X void kv_dec_free(uintptr_t d) { (void)d; }

X int kv_decode(uintptr_t d, const uint8_t *data, unsigned n, uintptr_t *out) {
	for (int i = 0; i < 8; i++) out[i] = d + data[i % n] + (uintptr_t)i;
	return (int)n;
}
