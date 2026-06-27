//go:build darwin

#import <Foundation/Foundation.h>
#import <AppKit/AppKit.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#include <string.h>
#include <stdlib.h>

// Реализованы в Go: видео-кадр (BGRA) и аудио-чанк (interleaved float32).
extern void goSCKFrame(int handle, void *buf, int len);
extern void goSCKAudio(int handle, void *buf, int len);

// Старт SCStream трогает CoreGraphics/WindowServer, который в «голом» CLI не
// инициализирован (Assertion CGS_REQUIRE_INIT). NSApplicationLoad поднимает
// связь с WindowServer для не-бандл процесса. Делаем один раз.
static void sck_ensure_app(void) {
	static dispatch_once_t once;
	dispatch_once(&once, ^{
		NSApplicationLoad();
	});
}

// Синхронно получает доступный контент (SCShareableContent — асинхронный API).
static SCShareableContent *sck_fetch_content(void) {
	__block SCShareableContent *res = nil;
	dispatch_semaphore_t sem = dispatch_semaphore_create(0);
	[SCShareableContent
		getShareableContentExcludingDesktopWindows:NO
		                       onScreenWindowsOnly:YES
		                         completionHandler:^(SCShareableContent *c, NSError *e) {
			res = c;
			dispatch_semaphore_signal(sem);
		}];
	dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
	return res;
}

// sck_list_sources перечисляет доступные источники захвата через
// ScreenCaptureKit и возвращает JSON-строку {displays, windows, apps}.
// Вызывающий обязан free() результат.
char *sck_list_sources(void) {
	@autoreleasepool {
		NSDictionary *result = @{};
		SCShareableContent *content = sck_fetch_content();
		{
			if (content) {
				NSMutableArray *displays = [NSMutableArray array];
				for (SCDisplay *d in content.displays) {
					[displays addObject:@{
						@"id": @(d.displayID),
						@"width": @(d.width),
						@"height": @(d.height),
					}];
				}

				NSMutableArray *windows = [NSMutableArray array];
				for (SCWindow *w in content.windows) {
					NSString *title = w.title ?: @"";
					NSString *app = w.owningApplication.applicationName ?: @"";
					// Пропускаем безымянные служебные окна.
					if (title.length == 0 && app.length == 0) {
						continue;
					}
					[windows addObject:@{
						@"id": @(w.windowID),
						@"title": title,
						@"app": app,
						@"width": @((int)w.frame.size.width),
						@"height": @((int)w.frame.size.height),
					}];
				}

				NSMutableArray *apps = [NSMutableArray array];
				NSMutableSet *seenApps = [NSMutableSet set];
				for (SCRunningApplication *a in content.applications) {
					NSString *name = a.applicationName ?: @"";
					if (name.length == 0) {
						continue;
					}
					// Только «обычные» приложения (есть в доке) — отсекаем Dock,
					// Control Center, Wallpaper, хелперы и т.п. Плюс дедуп по pid.
					NSRunningApplication *ra =
						[NSRunningApplication runningApplicationWithProcessIdentifier:a.processID];
					if (ra && ra.activationPolicy != NSApplicationActivationPolicyRegular) {
						continue;
					}
					NSNumber *pid = @(a.processID);
					if ([seenApps containsObject:pid]) {
						continue;
					}
					[seenApps addObject:pid];
					[apps addObject:@{
						@"pid": pid,
						@"bundleId": a.bundleIdentifier ?: @"",
						@"name": name,
					}];
				}

				result = @{ @"displays": displays, @"windows": windows, @"apps": apps };
			}
		}

		NSData *data = [NSJSONSerialization dataWithJSONObject:result options:0 error:nil];
		NSString *json = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
		return strdup(json.UTF8String);
	}
}

// --- Захват выбранного источника ---

// Делегат потока: на каждый кадр копирует BGRA плотно (без stride-паддинга)
// и отдаёт его в Go через goSCKFrame.
@interface KatanaOutput : NSObject <SCStreamOutput, SCStreamDelegate>
@property (nonatomic) int handle;
@end

@implementation KatanaOutput
- (void)stream:(SCStream *)stream
	didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
	               ofType:(SCStreamOutputType)type {
	if (!CMSampleBufferIsValid(sampleBuffer)) {
		return;
	}
	if (type == SCStreamOutputTypeAudio) {
		[self handleAudio:sampleBuffer];
		return;
	}
	if (type != SCStreamOutputTypeScreen) {
		return;
	}
	CVImageBufferRef px = CMSampleBufferGetImageBuffer(sampleBuffer);
	if (!px) {
		return;
	}
	CVPixelBufferLockBaseAddress(px, kCVPixelBufferLock_ReadOnly);
	size_t w = CVPixelBufferGetWidth(px);
	size_t h = CVPixelBufferGetHeight(px);
	size_t stride = CVPixelBufferGetBytesPerRow(px);
	uint8_t *base = (uint8_t *)CVPixelBufferGetBaseAddress(px);
	size_t rowBytes = w * 4;
	uint8_t *tight = malloc(rowBytes * h);
	if (tight && base) {
		for (size_t y = 0; y < h; y++) {
			memcpy(tight + y * rowBytes, base + y * stride, rowBytes);
		}
	}
	CVPixelBufferUnlockBaseAddress(px, kCVPixelBufferLock_ReadOnly);
	if (tight) {
		goSCKFrame(self.handle, tight, (int)(rowBytes * h));
		free(tight);
	}
}
// handleAudio извлекает PCM из аудио-CMSampleBuffer и отдаёт в Go как
// interleaved float32 (стерео) — формат, который понимает ffmpeg (-f f32le).
- (void)handleAudio:(CMSampleBufferRef)sampleBuffer {
	const AudioStreamBasicDescription *asbd = NULL;
	CMFormatDescriptionRef fmt = CMSampleBufferGetFormatDescription(sampleBuffer);
	if (fmt) {
		asbd = CMAudioFormatDescriptionGetStreamBasicDescription((CMAudioFormatDescriptionRef)fmt);
	}
	if (!asbd || asbd->mBitsPerChannel != 32) {
		return; // ожидаем float32 (SCK отдаёт planar float32 48k стерео)
	}
	int channels = asbd->mChannelsPerFrame;
	if (channels < 1 || channels > 8) {
		return;
	}
	BOOL planar = (asbd->mFormatFlags & kAudioFormatFlagIsNonInterleaved) != 0;

	// AudioBufferList должен вмещать по буферу на канал (для planar). Базовый
	// struct содержит место под 1 буфер — добавляем (channels-1).
	char storage[sizeof(AudioBufferList) + 7 * sizeof(AudioBuffer)];
	AudioBufferList *abl = (AudioBufferList *)storage;
	size_t ablSize = sizeof(AudioBufferList) + (size_t)(channels - 1) * sizeof(AudioBuffer);

	CMBlockBufferRef block = NULL;
	OSStatus st = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
		sampleBuffer, NULL, abl, ablSize, NULL, NULL,
		kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment, &block);
	if (st != noErr) {
		static BOOL ablLogged = NO;
		if (!ablLogged) { ablLogged = YES; NSLog(@"katana: audio buffer list err=%d", (int)st); }
		return;
	}

	if (!planar || abl->mNumberBuffers == 1) {
		// Interleaved — отдаём как есть.
		goSCKAudio(self.handle, abl->mBuffers[0].mData, (int)abl->mBuffers[0].mDataByteSize);
	} else {
		// Planar (по буферу на канал) → интерливим в [L R L R ...].
		int frames = (int)(abl->mBuffers[0].mDataByteSize / sizeof(float));
		float *out = malloc((size_t)frames * channels * sizeof(float));
		if (out) {
			for (int i = 0; i < frames; i++) {
				for (int c = 0; c < channels; c++) {
					out[i * channels + c] = ((float *)abl->mBuffers[c].mData)[i];
				}
			}
			goSCKAudio(self.handle, out, frames * channels * (int)sizeof(float));
			free(out);
		}
	}
	if (block) {
		CFRelease(block);
	}
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
}
@end

static NSMutableDictionary<NSNumber *, SCStream *> *gStreams;
static NSMutableDictionary<NSNumber *, KatanaOutput *> *gOutputs;
static NSMutableDictionary<NSNumber *, SCStreamConfiguration *> *gConfigs;
static dispatch_queue_t gQueue;
static dispatch_queue_t gAudioQueue;

// sck_source_size возвращает размер источника в пикселях (без старта потока),
// чтобы вызывающий мог настроить ffmpeg на нужный video_size.
// kind: 1=window, 2=app, иначе display. Возвращает 0 при успехе.
int sck_source_size(int kind, unsigned int sid, int *outW, int *outH) {
	@autoreleasepool {
		SCShareableContent *content = sck_fetch_content();
		if (!content) {
			return 1;
		}
		int w = 0, h = 0;
		if (kind == 1) {
			for (SCWindow *x in content.windows) {
				if (x.windowID == sid) {
					w = (int)x.frame.size.width;
					h = (int)x.frame.size.height;
					break;
				}
			}
		} else if (kind == 2) {
			SCDisplay *d = content.displays.firstObject;
			if (d) {
				w = (int)d.width;
				h = (int)d.height;
			}
		} else {
			for (SCDisplay *d in content.displays) {
				if (d.displayID == sid) {
					w = (int)d.width;
					h = (int)d.height;
					break;
				}
			}
		}
		if (w <= 0 || h <= 0) {
			return 2;
		}
		if (outW) *outW = w;
		if (outH) *outH = h;
		return 0;
	}
}

// sck_start запускает поток захвата выбранного источника. Кадры идут в
// goSCKFrame(handle, ...). Возвращает 0 при успехе.
int sck_start(int kind, unsigned int sid, int fps, int handle, int audio, int cursor) {
	@autoreleasepool {
		sck_ensure_app();
		SCShareableContent *content = sck_fetch_content();
		if (!content) {
			return 1;
		}

		SCContentFilter *filter = nil;
		int w = 0, h = 0;
		if (kind == 1) { // окно
			SCWindow *win = nil;
			for (SCWindow *x in content.windows) {
				if (x.windowID == sid) { win = x; break; }
			}
			if (!win) return 2;
			filter = [[SCContentFilter alloc] initWithDesktopIndependentWindow:win];
			w = (int)win.frame.size.width;
			h = (int)win.frame.size.height;
		} else if (kind == 2) { // приложение
			SCRunningApplication *app = nil;
			for (SCRunningApplication *a in content.applications) {
				if (a.processID == (pid_t)sid) { app = a; break; }
			}
			if (!app) return 3;
			SCDisplay *disp = content.displays.firstObject;
			if (!disp) return 4;
			filter = [[SCContentFilter alloc] initWithDisplay:disp
			                            includingApplications:@[ app ]
			                                  exceptingWindows:@[]];
			w = (int)disp.width;
			h = (int)disp.height;
		} else { // дисплей
			SCDisplay *disp = nil;
			for (SCDisplay *d in content.displays) {
				if (d.displayID == sid) { disp = d; break; }
			}
			if (!disp) disp = content.displays.firstObject;
			if (!disp) return 5;
			filter = [[SCContentFilter alloc] initWithDisplay:disp excludingWindows:@[]];
			w = (int)disp.width;
			h = (int)disp.height;
		}
		if (w <= 0 || h <= 0) return 6;

		SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
		cfg.width = w;
		cfg.height = h;
		cfg.minimumFrameInterval = CMTimeMake(1, fps > 0 ? fps : 30);
		cfg.pixelFormat = kCVPixelFormatType_32BGRA;
		cfg.queueDepth = 5;
		cfg.showsCursor = cursor ? YES : NO;
		if (audio) {
			cfg.capturesAudio = YES;
			cfg.sampleRate = 48000;
			cfg.channelCount = 2;
		}

		KatanaOutput *out = [[KatanaOutput alloc] init];
		out.handle = handle;
		SCStream *stream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:out];

		if (!gStreams) {
			gStreams = [NSMutableDictionary dictionary];
			gOutputs = [NSMutableDictionary dictionary];
			gConfigs = [NSMutableDictionary dictionary];
			gQueue = dispatch_queue_create("katana.sck.video", DISPATCH_QUEUE_SERIAL);
			gAudioQueue = dispatch_queue_create("katana.sck.audio", DISPATCH_QUEUE_SERIAL);
		}

		NSError *addErr = nil;
		if (![stream addStreamOutput:out
		                        type:SCStreamOutputTypeScreen
		          sampleHandlerQueue:gQueue
		                       error:&addErr]) {
			return 7;
		}
		if (audio) {
			NSError *aerr = nil;
			// Если аудио-выход не добавился — продолжаем без звука, не валим поток.
			BOOL ok = [stream addStreamOutput:out
			                             type:SCStreamOutputTypeAudio
			               sampleHandlerQueue:gAudioQueue
			                            error:&aerr];
			if (!ok) {
				NSLog(@"katana: add audio output failed: %@", aerr);
			}
		}

		__block int startErr = 0;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[stream startCaptureWithCompletionHandler:^(NSError *e) {
			if (e) startErr = 8;
			dispatch_semaphore_signal(sem);
		}];
		dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
		if (startErr) return startErr;

		gStreams[@(handle)] = stream;
		gOutputs[@(handle)] = out;
		gConfigs[@(handle)] = cfg;
		return 0;
	}
}

// sck_set_cursor меняет видимость курсора хоста в захвате НА ЛЕТУ
// (updateConfiguration, без перезапуска потока). Возвращает 0 при успехе.
int sck_set_cursor(int handle, int show) {
	@autoreleasepool {
		SCStream *stream = gStreams[@(handle)];
		SCStreamConfiguration *cfg = gConfigs[@(handle)];
		if (!stream || !cfg) {
			return 1;
		}
		cfg.showsCursor = show ? YES : NO;
		[stream updateConfiguration:cfg completionHandler:^(NSError *e) {
		}];
		return 0;
	}
}

// sck_source_rect возвращает глобальный прямоугольник источника (origin+size,
// в точках, top-left) — для маппинга координат мыши. kind: 1=window, 2=app,
// иначе display. Возвращает 0 при успехе.
int sck_source_rect(int kind, unsigned int sid, double *x, double *y, double *w, double *h) {
	@autoreleasepool {
		SCShareableContent *content = sck_fetch_content();
		if (!content) {
			return 1;
		}
		CGRect r = CGRectNull;
		if (kind == 1) {
			for (SCWindow *win in content.windows) {
				if (win.windowID == sid) { r = win.frame; break; }
			}
		} else if (kind == 2) {
			SCDisplay *d = content.displays.firstObject;
			if (d) r = d.frame;
		} else {
			for (SCDisplay *d in content.displays) {
				if (d.displayID == sid) { r = d.frame; break; }
			}
			if (CGRectIsNull(r) && content.displays.firstObject) {
				r = content.displays.firstObject.frame;
			}
		}
		if (CGRectIsNull(r) || r.size.width <= 0 || r.size.height <= 0) {
			return 2;
		}
		*x = r.origin.x;
		*y = r.origin.y;
		*w = r.size.width;
		*h = r.size.height;
		return 0;
	}
}

// inject_scroll постит пиксельно-точный скролл (как трекпад): dy — вертикаль,
// dx — горизонталь, в пикселях. Требует Accessibility.
void inject_scroll(int dx, int dy) {
	CGEventRef ev = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitPixel, 2, dy, dx);
	if (ev) {
		CGEventPost(kCGHIDEventTap, ev);
		CFRelease(ev);
	}
}

// activate_app выводит приложение (по pid) на передний план на хосте.
int activate_app(int pid) {
	@autoreleasepool {
		NSRunningApplication *app =
			[NSRunningApplication runningApplicationWithProcessIdentifier:(pid_t)pid];
		if (!app) {
			return 1;
		}
		BOOL ok = [app activateWithOptions:NSApplicationActivateAllWindows];
		return ok ? 0 : 2;
	}
}

// sck_stop останавливает поток захвата.
void sck_stop(int handle) {
	@autoreleasepool {
		SCStream *stream = gStreams[@(handle)];
		if (!stream) return;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[stream stopCaptureWithCompletionHandler:^(NSError *e) {
			dispatch_semaphore_signal(sem);
		}];
		dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
		[gStreams removeObjectForKey:@(handle)];
		[gOutputs removeObjectForKey:@(handle)];
		[gConfigs removeObjectForKey:@(handle)];
	}
}
