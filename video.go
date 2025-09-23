package reisen

// #cgo pkg-config: libavutil libavformat libavcodec libswscale libavfilter
// #include <libavcodec/avcodec.h>
// #include <libavformat/avformat.h>
// #include <libavutil/avutil.h>
// #include <libavutil/imgutils.h>
// #include <libavutil/opt.h>
// #include <libswscale/swscale.h>
// #include <libavfilter/avfilter.h>
// #include <libavfilter/buffersrc.h>
// #include <libavfilter/buffersink.h>
// #include <inttypes.h>
import "C"
import (
	"fmt"
	"unsafe"
)

// VideoStream is a streaming holding
// video frames.
type VideoStream struct {
	baseStream
	swsCtx    *C.struct_SwsContext
	rgbaFrame *C.AVFrame
	bufSize   C.int

	// libavfilter graph (frame-level)
	filterGraph     *C.AVFilterGraph
	bufSrcCtx       *C.AVFilterContext
	bufSinkCtx      *C.AVFilterContext
	filteredFrame   *C.AVFrame
	filterGraphDesc string
}

// AspectRatio returns the fraction of the video
// stream frame aspect ratio (1/0 if unknown).
func (video *VideoStream) AspectRatio() (int, int) {
	return int(video.codecParams.sample_aspect_ratio.num),
		int(video.codecParams.sample_aspect_ratio.den)
}

// Width returns the width of the video
// stream frame.
func (video *VideoStream) Width() int {
	return int(video.codecParams.width)
}

// Height returns the height of the video
// stream frame.
func (video *VideoStream) Height() int {
	return int(video.codecParams.height)
}

// OpenDecode opens the video stream for
// decoding with default parameters.
func (video *VideoStream) Open() error {
	return video.OpenDecode(
		int(video.codecParams.width),
		int(video.codecParams.height),
		InterpolationBicubic)
}

// OpenDecode opens the video stream for
// decoding with the specified parameters.
func (video *VideoStream) OpenDecode(width, height int, alg InterpolationAlgorithm) error {
	err := video.open()

	if err != nil {
		return err
	}

	video.rgbaFrame = C.av_frame_alloc()

	if video.rgbaFrame == nil {
		return fmt.Errorf(
			"couldn't allocate a new RGBA frame")
	}

	video.bufSize = C.av_image_get_buffer_size(
		C.AV_PIX_FMT_RGBA, C.int(width), C.int(height), 1)

	if video.bufSize < 0 {
		return fmt.Errorf(
			"%d: couldn't get the buffer size", video.bufSize)
	}

	buf := (*C.uint8_t)(unsafe.Pointer(
		C.av_malloc(bufferSize(video.bufSize))))

	if buf == nil {
		return fmt.Errorf(
			"couldn't allocate an AV buffer")
	}

	status := C.av_image_fill_arrays(&video.rgbaFrame.data[0],
		&video.rgbaFrame.linesize[0], buf, C.AV_PIX_FMT_RGBA,
		C.int(width), C.int(height), 1)

	if status < 0 {
		return fmt.Errorf(
			"%d: couldn't fill the image arrays", status)
	}

	video.swsCtx = C.sws_getContext(video.codecCtx.width,
		video.codecCtx.height, video.codecCtx.pix_fmt,
		C.int(width), C.int(height),
		C.AV_PIX_FMT_RGBA, C.int(alg), nil, nil, nil)

	if video.swsCtx == nil {
		return fmt.Errorf(
			"couldn't create an SWS context")
	}

	return nil
}

// ApplyVideoFilterGraph builds and enables a libavfilter graph that will run
// on decoded frames (e.g. "scale=640:480:flags=bilinear,format=rgba").
//
// Call this AFTER Open()/OpenDecode(). Any existing video filtergraph is replaced.
func (video *VideoStream) ApplyVideoFilterGraph(graph string) error {
	// Tear down any previous graph
	video.RemoveVideoFilterGraph()

	// Allocate graph
	video.filterGraph = C.avfilter_graph_alloc()
	if video.filterGraph == nil {
		return fmt.Errorf("couldn't allocate AVFilterGraph")
	}

	// Prepare buffer source (input) and buffer sink (output)
	bufSrc := C.avfilter_get_by_name(C.CString("buffer"))
	bufSink := C.avfilter_get_by_name(C.CString("buffersink"))
	if bufSrc == nil || bufSink == nil {
		video.RemoveVideoFilterGraph()
		return fmt.Errorf("missing buffer/buffersink filters in libavfilter")
	}

	// Build args for "buffer"
	tbNum := video.inner.time_base.num
	tbDen := video.inner.time_base.den
	sarNum := video.codecCtx.sample_aspect_ratio.num
	sarDen := video.codecCtx.sample_aspect_ratio.den
	if sarNum == 0 || sarDen == 0 {
		// default to 1:1 if not set
		sarNum, sarDen = 1, 1
	}

	args := fmt.Sprintf(
		"video_size=%dx%d:pix_fmt=%d:time_base=%d/%d:pixel_aspect=%d/%d",
		int(video.codecCtx.width), int(video.codecCtx.height),
		int(video.codecCtx.pix_fmt),
		int(tbNum), int(tbDen),
		int(sarNum), int(sarDen),
	)

	csArgs := C.CString(args)
	defer C.free(unsafe.Pointer(csArgs))
	nameIn := C.CString("in")
	defer C.free(unsafe.Pointer(nameIn))
	nameOut := C.CString("out")
	defer C.free(unsafe.Pointer(nameOut))

	var ret C.int
	ret = C.avfilter_graph_create_filter(
		&video.bufSrcCtx, bufSrc, nameIn, csArgs, nil, video.filterGraph)
	if ret < 0 {
		video.RemoveVideoFilterGraph()
		return fmt.Errorf("%d: couldn't create buffer source", int(ret))
	}

	ret = C.avfilter_graph_create_filter(
		&video.bufSinkCtx, bufSink, nameOut, nil, nil, video.filterGraph)
	if ret < 0 {
		video.RemoveVideoFilterGraph()
		return fmt.Errorf("%d: couldn't create buffer sink", int(ret))
	}

	// Wrap our endpoints for graph parsing: [in] -> graph -> [out]
	inouts := C.avfilter_inout_alloc()
	outputs := C.avfilter_inout_alloc()
	if inouts == nil || outputs == nil {
		if inouts != nil {
			C.avfilter_inout_free(&inouts)
		}
		if outputs != nil {
			C.avfilter_inout_free(&outputs)
		}
		video.RemoveVideoFilterGraph()
		return fmt.Errorf("couldn't allocate AVFilterInOut")
	}
	defer C.avfilter_inout_free(&inouts)
	defer C.avfilter_inout_free(&outputs)

	// outputs: our source node named "in"
	outputs.name = C.av_strdup(nameIn)
	outputs.filter_ctx = video.bufSrcCtx
	outputs.pad_idx = 0
	outputs.next = nil

	// inputs: our sink node named "out"
	inouts.name = C.av_strdup(nameOut)
	inouts.filter_ctx = video.bufSinkCtx
	inouts.pad_idx = 0
	inouts.next = nil

	csGraph := C.CString(graph)
	defer C.free(unsafe.Pointer(csGraph))
	ret = C.avfilter_graph_parse_ptr(video.filterGraph, csGraph, &inouts, &outputs, nil)
	if ret < 0 {
		video.RemoveVideoFilterGraph()
		return fmt.Errorf("%d: couldn't parse filter graph", int(ret))
	}

	ret = C.avfilter_graph_config(video.filterGraph, nil)
	if ret < 0 {
		video.RemoveVideoFilterGraph()
		return fmt.Errorf("%d: couldn't configure filter graph", int(ret))
	}

	// Allocate a reusable frame for filtered output
	video.filteredFrame = C.av_frame_alloc()
	if video.filteredFrame == nil {
		video.RemoveVideoFilterGraph()
		return fmt.Errorf("couldn't allocate filtered frame")
	}

	video.filterGraphDesc = graph
	return nil
}

// RemoveVideoFilterGraph frees the current libavfilter graph (if any).
func (video *VideoStream) RemoveVideoFilterGraph() {
	if video.filteredFrame != nil {
		C.av_frame_free(&video.filteredFrame)
		video.filteredFrame = nil
	}
	if video.filterGraph != nil {
		// Will free all contained filter contexts as well.
		C.avfilter_graph_free(&video.filterGraph)
		video.filterGraph = nil
		video.bufSrcCtx = nil
		video.bufSinkCtx = nil
		video.filterGraphDesc = ""
	}
}

// ReadFrame reads the next frame from the stream.
func (video *VideoStream) ReadFrame() (Frame, bool, error) {
	return video.ReadVideoFrame()
}

// ReadVideoFrame reads the next video frame
// from the video stream.
func (video *VideoStream) ReadVideoFrame() (*VideoFrame, bool, error) {
	ok, err := video.read()

	if err != nil {
		return nil, false, err
	}

	if ok && video.skip {
		return nil, true, nil
	}

	// No more data.
	if !ok {
		return nil, false, nil
	}

	// If a libavfilter graph is active, run frame through it and return filtered RGBA.
	if video.filterGraph != nil && video.bufSrcCtx != nil && video.bufSinkCtx != nil && video.filteredFrame != nil {
		// Push decoded frame into the graph
		if ret := C.av_buffersrc_add_frame_flags(video.bufSrcCtx, video.frame, C.AV_BUFFERSRC_FLAG_KEEP_REF); ret < 0 {
			return nil, false, fmt.Errorf("%d: couldn't add frame to buffersrc", int(ret))
		}
		// Pull filtered frame
		if ret := C.av_buffersink_get_frame(video.bufSinkCtx, video.filteredFrame); ret < 0 {
			// EAGAIN should be rare here; treat as no frame available
			return nil, true, nil
		}

		// Extract RGBA bytes (stride-aware copy to a tight buffer)
		w := int(video.filteredFrame.width)
		h := int(video.filteredFrame.height)
		stride := int(video.filteredFrame.linesize[0])
		rowBytes := w * 4
		size := rowBytes * h
		buf := make([]byte, size)
		src := uintptr(unsafe.Pointer(video.filteredFrame.data[0]))
		for y := 0; y < h; y++ {
			dstOff := y * rowBytes
			srcPtr := unsafe.Pointer(src + uintptr(y*stride))
			copy(buf[dstOff:dstOff+rowBytes], C.GoBytes(srcPtr, C.int(rowBytes)))
		}
		// We’re done with the filtered frame contents
		C.av_frame_unref(video.filteredFrame)

		frame := newVideoFrame(video, int64(video.frame.pts),
			int(video.frame.coded_picture_number),
			int(video.frame.display_picture_number),
			w, h, buf)
		return frame, true, nil
	}

	// No filtergraph: fall back to sws_scale to RGBA at the decode size.
	C.sws_scale(video.swsCtx, &video.frame.data[0],
		&video.frame.linesize[0], 0,
		video.codecCtx.height,
		&video.rgbaFrame.data[0],
		&video.rgbaFrame.linesize[0])

	data := C.GoBytes(unsafe.Pointer(video.rgbaFrame.data[0]), video.bufSize)
	frame := newVideoFrame(video, int64(video.frame.pts),
		int(video.frame.coded_picture_number),
		int(video.frame.display_picture_number),
		int(video.codecCtx.width), int(video.codecCtx.height), data)

	return frame, true, nil
}

// Close closes the video stream for decoding.
func (video *VideoStream) Close() error {
	err := video.close()

	if err != nil {
		return err
	}

	// Free libavfilter graph (if any)
	video.RemoveVideoFilterGraph()

	C.av_free(unsafe.Pointer(video.rgbaFrame))
	video.rgbaFrame = nil
	C.sws_freeContext(video.swsCtx)
	video.swsCtx = nil

	return nil
}
