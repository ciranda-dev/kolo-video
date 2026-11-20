// Flat C ABI over libvpx, same settings as kolo-video/vpx/vpx.go.
// Only ints and pointers cross the boundary so wasm, cgo and purego can
// call it the same way.
#include <vpx/vpx_encoder.h>
#include <vpx/vpx_decoder.h>
#include <vpx/vp8cx.h>
#include <vpx/vp8dx.h>
#include <stdint.h>
#include <stdlib.h>

#define KV_ABI_VERSION 1

#if defined(__wasm__)
#define KV_EXPORT(n) __attribute__((export_name(#n)))
#else
#define KV_EXPORT(n) __attribute__((visibility("default")))
#endif

typedef struct {
	vpx_codec_ctx_t ctx;
	vpx_codec_enc_cfg_t cfg;
	vpx_codec_iter_t it;
} kv_enc;

typedef struct {
	vpx_codec_ctx_t ctx;
	vpx_image_t *img;
} kv_dec;

KV_EXPORT(kv_malloc) void *kv_malloc(size_t n) { return malloc(n); }
KV_EXPORT(kv_free) void kv_free(void *p) { free(p); }

static vpx_codec_err_t kv_enc_controls(kv_enc *e, int vp9, int threads, int speed, unsigned max_intra) {
	vpx_codec_ctx_t *c = &e->ctx;
	vpx_codec_err_t r = vpx_codec_control(c, VP8E_SET_CPUUSED, speed);
	if (r == VPX_CODEC_OK) r = vpx_codec_control(c, VP8E_SET_STATIC_THRESHOLD, 1);
	if (r == VPX_CODEC_OK) r = vpx_codec_control(c, VP8E_SET_MAX_INTRA_BITRATE_PCT, max_intra);
	if (vp9) {
		if (r == VPX_CODEC_OK) r = vpx_codec_control(c, VP9E_SET_AQ_MODE, 3);
		if (r == VPX_CODEC_OK) r = vpx_codec_control(c, VP9E_SET_FRAME_PARALLEL_DECODING, 0);
		if (r == VPX_CODEC_OK) r = vpx_codec_control(c, VP9E_SET_TILE_COLUMNS, threads >> 1);
		if (r == VPX_CODEC_OK) r = vpx_codec_control(c, VP9E_SET_ROW_MT, 1);
	} else {
		if (r == VPX_CODEC_OK) r = vpx_codec_control(c, VP8E_SET_TOKEN_PARTITIONS, VP8_ONE_TOKENPARTITION);
	}
	return r;
}

KV_EXPORT(kv_enc_new) kv_enc *kv_enc_new(int vp9, int w, int h, unsigned kbps, int threads, int speed, unsigned max_intra, int *err) {
	kv_enc *e = calloc(1, sizeof(kv_enc));
	if (e == NULL) {
		*err = VPX_CODEC_MEM_ERROR;
		return NULL;
	}
	vpx_codec_iface_t *iface = vp9 ? vpx_codec_vp9_cx() : vpx_codec_vp8_cx();
	*err = vpx_codec_enc_config_default(iface, &e->cfg, 0);
	if (*err != VPX_CODEC_OK) {
		free(e);
		return NULL;
	}
	vpx_codec_enc_cfg_t *c = &e->cfg;
	c->g_w = w;
	c->g_h = h;
	c->g_timebase.num = 1;
	c->g_timebase.den = 90000;
	c->g_threads = threads;
	c->g_lag_in_frames = 0;
	c->g_error_resilient = VPX_ERROR_RESILIENT_DEFAULT;
	c->g_pass = VPX_RC_ONE_PASS;
	c->rc_end_usage = VPX_CBR;
	c->rc_target_bitrate = kbps;
	c->rc_dropframe_thresh = 30;
	c->rc_resize_allowed = 0;
	c->rc_min_quantizer = 2;
	c->rc_max_quantizer = vp9 ? 52 : 56;
	c->rc_undershoot_pct = vp9 ? 50 : 100;
	c->rc_overshoot_pct = vp9 ? 50 : 15;
	c->rc_buf_initial_sz = 500;
	c->rc_buf_optimal_sz = 600;
	c->rc_buf_sz = 1000;
	c->kf_mode = VPX_KF_DISABLED;
	*err = vpx_codec_enc_init(&e->ctx, iface, c, 0);
	if (*err != VPX_CODEC_OK) {
		free(e);
		return NULL;
	}
	*err = kv_enc_controls(e, vp9, threads, speed, max_intra);
	if (*err != VPX_CODEC_OK) {
		vpx_codec_destroy(&e->ctx);
		free(e);
		return NULL;
	}
	return e;
}

KV_EXPORT(kv_enc_free) void kv_enc_free(kv_enc *e) {
	vpx_codec_destroy(&e->ctx);
	free(e);
}

KV_EXPORT(kv_encode) int kv_encode(kv_enc *e, uint8_t *y, int ys, uint8_t *u, uint8_t *v, int uvs, int64_t pts, uint32_t dur, int key) {
	vpx_image_t img;
	if (vpx_img_wrap(&img, VPX_IMG_FMT_I420, e->cfg.g_w, e->cfg.g_h, 1, y) == NULL) return VPX_CODEC_INVALID_PARAM;
	img.planes[VPX_PLANE_Y] = y;
	img.planes[VPX_PLANE_U] = u;
	img.planes[VPX_PLANE_V] = v;
	img.stride[VPX_PLANE_Y] = ys;
	img.stride[VPX_PLANE_U] = uvs;
	img.stride[VPX_PLANE_V] = uvs;
	e->it = NULL;
	return vpx_codec_encode(&e->ctx, &img, pts, dur, key ? VPX_EFLAG_FORCE_KF : 0, VPX_DL_REALTIME);
}

// kv_next_frame writes the next packet's pointer, size and keyframe flag
// into out[0..2] and returns 1, or returns 0 when there is none.
KV_EXPORT(kv_next_frame) int kv_next_frame(kv_enc *e, uintptr_t *out) {
	const vpx_codec_cx_pkt_t *p;
	while ((p = vpx_codec_get_cx_data(&e->ctx, &e->it)) != NULL) {
		if (p->kind != VPX_CODEC_CX_FRAME_PKT) continue;
		out[0] = (uintptr_t)p->data.frame.buf;
		out[1] = (uintptr_t)p->data.frame.sz;
		out[2] = (p->data.frame.flags & VPX_FRAME_IS_KEY) != 0;
		return 1;
	}
	return 0;
}

KV_EXPORT(kv_set_bitrate) int kv_set_bitrate(kv_enc *e, unsigned kbps) {
	unsigned old = e->cfg.rc_target_bitrate;
	e->cfg.rc_target_bitrate = kbps;
	vpx_codec_err_t r = vpx_codec_enc_config_set(&e->ctx, &e->cfg);
	if (r != VPX_CODEC_OK) e->cfg.rc_target_bitrate = old;
	return r;
}

KV_EXPORT(kv_dec_new) kv_dec *kv_dec_new(int vp9, int threads, int *err) {
	kv_dec *d = calloc(1, sizeof(kv_dec));
	if (d == NULL) {
		*err = VPX_CODEC_MEM_ERROR;
		return NULL;
	}
	vpx_codec_dec_cfg_t cfg = {0};
	cfg.threads = threads;
	*err = vpx_codec_dec_init(&d->ctx, vp9 ? vpx_codec_vp9_dx() : vpx_codec_vp8_dx(), &cfg, 0);
	if (*err != VPX_CODEC_OK) {
		free(d);
		return NULL;
	}
	return d;
}

KV_EXPORT(kv_dec_free) void kv_dec_free(kv_dec *d) {
	vpx_codec_destroy(&d->ctx);
	free(d);
}

// kv_decode decodes one packet and keeps the last shown frame. out gets
// width, height, the three plane pointers, the two strides and the
// corrupted flag; out[0] is 0 when no frame is shown.
KV_EXPORT(kv_decode) int kv_decode(kv_dec *d, const uint8_t *data, unsigned n, uintptr_t *out) {
	vpx_codec_err_t r = vpx_codec_decode(&d->ctx, data, n, NULL, 0);
	if (r != VPX_CODEC_OK) return r;
	vpx_codec_iter_t it = NULL;
	vpx_image_t *img, *last = NULL;
	while ((img = vpx_codec_get_frame(&d->ctx, &it)) != NULL) last = img;
	int c = 0;
	if (vpx_codec_control(&d->ctx, VP8D_GET_FRAME_CORRUPTED, &c) != VPX_CODEC_OK) c = 0;
	out[7] = c;
	if (last == NULL || last->fmt != VPX_IMG_FMT_I420) {
		out[0] = 0;
		return 0;
	}
	out[0] = last->d_w;
	out[1] = last->d_h;
	out[2] = (uintptr_t)last->planes[0];
	out[3] = (uintptr_t)last->planes[1];
	out[4] = (uintptr_t)last->planes[2];
	out[5] = last->stride[0];
	out[6] = last->stride[1];
	return 0;
}

KV_EXPORT(kv_version) const char *kv_version(void) { return vpx_codec_version_str(); }
KV_EXPORT(kv_abi_version) int kv_abi_version(void) { return KV_ABI_VERSION; }
