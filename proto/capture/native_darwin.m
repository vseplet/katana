//go:build darwin

// Нативное кодирование на macOS без ffmpeg: H264 через VideoToolbox
// (VTCompressionSession, аппаратный энкодер) и Opus через libopus. SCK отдаёт
// BGRA-кадры и PCM — мы кодируем их прямо в процессе.
#import <VideoToolbox/VideoToolbox.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <opus/opus.h>

// Реализована в Go: один закодированный H264 access unit (Annex-B).
extern void goVTFrame(int handle, void *buf, int len, int keyframe);

struct VTEnc {
	int handle;
	VTCompressionSessionRef session;
};

// appendAnnexB добавляет NAL с 4-байтовым старт-кодом в растущий буфер.
static void appendAnnexB(uint8_t **buf, size_t *len, size_t *cap, const uint8_t *nal, size_t nalLen) {
	static const uint8_t sc[4] = {0, 0, 0, 1};
	size_t need = *len + 4 + nalLen;
	if (need > *cap) {
		*cap = need * 2;
		*buf = realloc(*buf, *cap);
	}
	memcpy(*buf + *len, sc, 4);
	*len += 4;
	memcpy(*buf + *len, nal, nalLen);
	*len += nalLen;
}

// vtCallback — выход энкодера. Конвертируем AVCC (length-prefixed) в Annex-B,
// на ключевом кадре добавляем SPS/PPS, и отдаём кадр в Go.
static void vtCallback(void *refcon, void *src, OSStatus status,
                       VTEncodeInfoFlags flags, CMSampleBufferRef sb) {
	(void)src;
	(void)flags;
	if (status != noErr || sb == NULL || !CMSampleBufferDataIsReady(sb)) {
		return;
	}
	int handle = (int)(intptr_t)refcon;

	int keyframe = 1;
	CFArrayRef att = CMSampleBufferGetSampleAttachmentsArray(sb, false);
	if (att && CFArrayGetCount(att) > 0) {
		CFDictionaryRef d = (CFDictionaryRef)CFArrayGetValueAtIndex(att, 0);
		CFBooleanRef notSync = (CFBooleanRef)CFDictionaryGetValue(d, kCMSampleAttachmentKey_NotSync);
		if (notSync && CFBooleanGetValue(notSync)) {
			keyframe = 0;
		}
	}

	uint8_t *out = NULL;
	size_t outLen = 0, outCap = 0;

	if (keyframe) {
		CMFormatDescriptionRef fmt = CMSampleBufferGetFormatDescription(sb);
		size_t count = 0;
		int nalHdr = 0;
		if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, 0, NULL, NULL, &count, &nalHdr) == noErr) {
			for (size_t i = 0; i < count; i++) {
				const uint8_t *ps = NULL;
				size_t psLen = 0;
				if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, i, &ps, &psLen, NULL, NULL) == noErr) {
					appendAnnexB(&out, &outLen, &outCap, ps, psLen);
				}
			}
		}
	}

	CMBlockBufferRef bb = CMSampleBufferGetDataBuffer(sb);
	size_t total = 0;
	char *ptr = NULL;
	if (bb && CMBlockBufferGetDataPointer(bb, 0, NULL, &total, &ptr) == noErr) {
		size_t off = 0;
		while (off + 4 <= total) {
			uint32_t nalLen = ((uint8_t)ptr[off] << 24) | ((uint8_t)ptr[off + 1] << 16) |
			                  ((uint8_t)ptr[off + 2] << 8) | (uint8_t)ptr[off + 3];
			off += 4;
			if (nalLen == 0 || off + nalLen > total) {
				break;
			}
			appendAnnexB(&out, &outLen, &outCap, (const uint8_t *)ptr + off, nalLen);
			off += nalLen;
		}
	}

	if (out && outLen > 0) {
		goVTFrame(handle, out, (int)outLen, keyframe);
	}
	free(out);
}

// vt_open создаёт H264-энкодер (realtime, без B-кадров, кейфрейм раз в секунду).
int vt_open(int handle, int w, int h, int fps, int bitrateKbps, struct VTEnc **out) {
	struct VTEnc *e = calloc(1, sizeof(struct VTEnc));
	if (!e) {
		return -1;
	}
	e->handle = handle;
	OSStatus st = VTCompressionSessionCreate(kCFAllocatorDefault, w, h,
	                                         kCMVideoCodecType_H264, NULL, NULL, NULL,
	                                         vtCallback, (void *)(intptr_t)handle, &e->session);
	if (st != noErr) {
		free(e);
		return (int)st;
	}
	VTSessionSetProperty(e->session, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
	VTSessionSetProperty(e->session, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
	VTSessionSetProperty(e->session, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_High_AutoLevel);

	int br = bitrateKbps * 1000;
	CFNumberRef brn = CFNumberCreate(NULL, kCFNumberIntType, &br);
	VTSessionSetProperty(e->session, kVTCompressionPropertyKey_AverageBitRate, brn);
	CFRelease(brn);

	int gop = fps > 0 ? fps : 30;
	CFNumberRef gn = CFNumberCreate(NULL, kCFNumberIntType, &gop);
	VTSessionSetProperty(e->session, kVTCompressionPropertyKey_MaxKeyFrameInterval, gn);
	CFRelease(gn);
	double dur = 1.0;
	CFNumberRef dn = CFNumberCreate(NULL, kCFNumberDoubleType, &dur);
	VTSessionSetProperty(e->session, kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, dn);
	CFRelease(dn);

	VTCompressionSessionPrepareToEncodeFrames(e->session);
	*out = e;
	return 0;
}

// vt_encode заворачивает BGRA-кадр (tight, w*4 на строку) в CVPixelBuffer и
// кодирует. Результат уходит в vtCallback асинхронно.
int vt_encode(struct VTEnc *e, void *bgra, int w, int h, long long ptsNum, int fps) {
	if (!e || !e->session) {
		return -1;
	}
	CVPixelBufferRef pb = NULL;
	const void *keys[] = {kCVPixelBufferIOSurfacePropertiesKey};
	CFDictionaryRef empty = CFDictionaryCreate(NULL, NULL, NULL, 0,
	                                           &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	const void *vals[] = {empty};
	CFDictionaryRef attrs = CFDictionaryCreate(NULL, keys, vals, 1,
	                                           &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	OSStatus st = CVPixelBufferCreate(NULL, w, h, kCVPixelFormatType_32BGRA, attrs, &pb);
	CFRelease(attrs);
	CFRelease(empty);
	if (st != kCVReturnSuccess) {
		return (int)st;
	}
	CVPixelBufferLockBaseAddress(pb, 0);
	uint8_t *dst = (uint8_t *)CVPixelBufferGetBaseAddress(pb);
	size_t dstStride = CVPixelBufferGetBytesPerRow(pb);
	size_t srcStride = (size_t)w * 4;
	uint8_t *srcp = (uint8_t *)bgra;
	for (int y = 0; y < h; y++) {
		memcpy(dst + (size_t)y * dstStride, srcp + (size_t)y * srcStride, srcStride);
	}
	CVPixelBufferUnlockBaseAddress(pb, 0);

	CMTime pts = CMTimeMake(ptsNum, fps > 0 ? fps : 30);
	CMTime d = CMTimeMake(1, fps > 0 ? fps : 30);
	st = VTCompressionSessionEncodeFrame(e->session, pb, pts, d, NULL, NULL, NULL);
	CVPixelBufferRelease(pb);
	return (int)st;
}

void vt_close(struct VTEnc *e) {
	if (!e) {
		return;
	}
	if (e->session) {
		VTCompressionSessionCompleteFrames(e->session, kCMTimeInvalid);
		VTCompressionSessionInvalidate(e->session);
		CFRelease(e->session);
	}
	free(e);
}

// --- Opus ---

// opus_enc_create — энкодер 48k/стерео, низкая задержка, 128 kbps.
int opus_enc_create(int rate, int channels, OpusEncoder **out) {
	int err = 0;
	OpusEncoder *enc = opus_encoder_create(rate, channels, OPUS_APPLICATION_RESTRICTED_LOWDELAY, &err);
	if (err != OPUS_OK || !enc) {
		return err ? err : -1;
	}
	opus_encoder_ctl(enc, OPUS_SET_BITRATE(128000));
	*out = enc;
	return 0;
}

// opus_enc_frame кодирует frameSize сэмплов на канал (interleaved float). Возврат:
// число байт пакета (>0) или код ошибки (<0).
int opus_enc_frame(OpusEncoder *enc, const float *pcm, int frameSize, unsigned char *out, int maxBytes) {
	return opus_encode_float(enc, pcm, frameSize, out, maxBytes);
}

void opus_enc_destroy(OpusEncoder *enc) {
	if (enc) {
		opus_encoder_destroy(enc);
	}
}
