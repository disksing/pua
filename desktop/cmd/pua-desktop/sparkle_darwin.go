//go:build darwin && sparkle

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework Sparkle
#import <Sparkle/Sparkle.h>

static SPUStandardUpdaterController *puaSparkleController;

static void startPUASparkle(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		puaSparkleController = [[SPUStandardUpdaterController alloc]
			initWithStartingUpdater:YES updaterDelegate:nil userDriverDelegate:nil];
	});
}

static void checkPUASparkle(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[puaSparkleController.updater checkForUpdates];
	});
}
*/
import "C"

func startSparkle() {
	C.startPUASparkle()
}

func checkSparkle() { C.checkPUASparkle() }
