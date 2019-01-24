package ninjamencoder

import (
	"unsafe"
	"reflect"
	"fmt"

	"github.com/xlab/vorbis-go/vorbis"

	logrus "github.com/sirupsen/logrus"
)

var log = logrus.New()

// stuff internal to encoder that we need to pass around
type vorbisParam struct {
	info        vorbis.Info
	streamState vorbis.OggStreamState
	comment     vorbis.Comment
	dspState    vorbis.DspState
	block       vorbis.Block
}

// Encoder parameters to use for vorbis encoder.
type Encoder struct {
	ChannelCount int         // number of audio channels to encode
	SampleRate   int         // audio sample rate
	ChunkSize    int         // number of frames in a single packet
	Quality      float32     // vorbis encoder quality
	vorbis       vorbisParam // internal vorbis parameters
}

// NewEncoder creates a new Encoder struct with sensible default values:
// stereo 44.1kHz, chunk size set to 8K frames, quality set to 0.1
func NewEncoder() *Encoder {
	return &Encoder{
		2,     // default to stereo
		44100, // default to 44.1kHz sample rate
		8192,  // default to splitting interval into packets 8K frames each
		0.1,   // default to quality of 0.1
		vorbisParam{},
	}
}

// generics ftw
func intmin(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func SetLogLevel(level logrus.Level) {
	log.SetLevel(level)
}

func DeinterleaveSamples(samples []float32, channelCount int) ([][]float32, error) {
	nFrames := len(samples) / channelCount

	if len(samples) != (nFrames * channelCount) {
		return nil, fmt.Errorf("Invalid number of samples")
	}

	res := make([][]float32, channelCount)
	for c := 0; c < channelCount; c++ {
		res[c] = make([]float32, nFrames)

		for f := 0; f < nFrames; f++ {
			// not terribly cache efficient but oh well
			res[c][f] = samples[f * channelCount + c]
		}
	}
	return res, nil
}

// this is totally going to work, pinky swear
func getSamplePtr(data **float32, channelIndex int, sampleIndex int) *float32 {
	// &data[0]
	basePtr := unsafe.Pointer(data)
	// calculate offset to get from 0 to channelIndex
	baseOffset := uintptr(channelIndex) * unsafe.Sizeof(*data)
	// *(&data[channelIndex])
	// adding offset to base will only get us &data[channelIndex], so we
	// need an additional dereferencing to get to data[channelIndex]
	samplesPtr := *(*uintptr)(unsafe.Pointer(uintptr(basePtr) + baseOffset))

	// we've blasted through one level of indirection, round two
	// we start with &data[channelIndex][0]

	// calculate offset to get from 0 to sampleIndex
	sampleOffset := uintptr(sampleIndex) * unsafe.Sizeof(**data)
	// &data[channelIndex][sampleIndex]
	samplePtr := unsafe.Pointer(samplesPtr + sampleOffset)

	// go vet says the above is misuse of unsafe pointers, but this is
	// intentional - samplesPtr points to an address, so it needs
	// dereferencing before we can add an offset

	// (float*)&data[channelIndex][sampleIndex]
	return (*float32)(samplePtr)
}

func (encoder *Encoder) analyzeSamples(samples [][]float32) {
	// vorbis analysis buffer is managed by vorbis library
	nFrames := len(samples[0])

	res := vorbis.AnalysisBuffer(&encoder.vorbis.dspState, int32(nFrames))

	log.Debugf("Analyzing %v frames", nFrames)

	for i := 0; i < nFrames; i++ {
		for c := 0; c < encoder.ChannelCount; c++ {
			// get a ptr to sample value, and write it
			samplePtr := getSamplePtr(res, c, i)
			*samplePtr = samples[c][i]
		}
	}

	// notify vorbis that we've wrote the analysis buffer
	vorbis.AnalysisWrote(&encoder.vorbis.dspState, int32(nFrames))
}

func resizeByteSlice(slice *[]byte, sliceLen int) {
	sPtr := (*reflect.SliceHeader)(unsafe.Pointer(slice))
	sPtr.Len = sliceLen
}

func (encoder *Encoder) initVorbisHeaders() ([]byte, error) {
	var headers []byte

	log.Debug("Initializing vorbis headers")

	vorbis.InfoInit(&encoder.vorbis.info)
	ret := vorbis.EncodeInitVbr(&encoder.vorbis.info,
		encoder.ChannelCount,
		encoder.SampleRate,
		encoder.Quality)
	if ret != 0 {
		return nil, fmt.Errorf("EncodeInitVbr returned %v", ret)
	}

	vorbis.CommentInit(&encoder.vorbis.comment)
	vorbis.CommentAddTag(&encoder.vorbis.comment, "Encoder", "burillo-se/ninjamencoder")

	ret = vorbis.AnalysisInit(&encoder.vorbis.dspState, &encoder.vorbis.info)
	if ret != 0 {
		return nil, fmt.Errorf("AnalysisInit returned %v", ret)
	}
	ret = vorbis.BlockInit(&encoder.vorbis.dspState, &encoder.vorbis.block)
	if ret != 0 {
		return nil, fmt.Errorf("BlockInit returned %v", ret)
	}

	ret = vorbis.OggStreamInit(&encoder.vorbis.streamState, 0)
	if ret != 0 {
		return nil, fmt.Errorf("OggStreamInit returned %v", ret)
	}

	var header vorbis.OggPacket
	headerComm := make([]vorbis.OggPacket, 1)
	headerCode := make([]vorbis.OggPacket, 1)

	defer header.Free()
	defer headerComm[0].Free()
	defer headerCode[0].Free()

	ret = vorbis.AnalysisHeaderout(&encoder.vorbis.dspState, &encoder.vorbis.comment,
		&header, headerComm, headerCode)
	if ret != 0 {
		return nil, fmt.Errorf("AnalysisHeaderout returned %v", ret)
	}

	ret = vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &header)
	if ret != 0 {
		return nil, fmt.Errorf("OggStreamPacketin(&header) returned %v", ret)
	}
	ret = vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &headerComm[0])
	if ret != 0 {
		return nil, fmt.Errorf("OggStreamPacketin(&headerComm[0]) returned %v", ret)
	}
	ret = vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &headerCode[0])
	if ret != 0 {
		return nil, fmt.Errorf("OggStreamPacketin(&headerCode[0]) returned %v", ret)
	}

	for {
		page := vorbis.OggPage{}
		defer page.Free()
		result := vorbis.OggStreamFlush(&encoder.vorbis.streamState, &page)
		if result == 0 {
			log.Debug("Stream write complete")
			break
		}
		// ensure that page data is in sync with its C counterpart
		page.Deref()
		// modify slice length to be equal to one we expect
		resizeByteSlice(&page.Header, page.HeaderLen)
		resizeByteSlice(&page.Body, page.BodyLen)

		headers = append(headers, page.Header...)
		headers = append(headers, page.Body...)

		log.Debugf("Appending %v bytes to headers", len(page.Header) + len(page.Body))
		log.Tracef("Page contents: %v", page)
	}

	return headers, nil
}

func (encoder *Encoder) clearVorbisHeaders() {
	log.Debug("Clearing vorbis encoder state")
	vorbis.OggStreamClear(&encoder.vorbis.streamState)
	vorbis.BlockClear(&encoder.vorbis.block)
	vorbis.DspClear(&encoder.vorbis.dspState)
	vorbis.CommentClear(&encoder.vorbis.comment)
	vorbis.InfoClear(&encoder.vorbis.info)

	encoder.vorbis.streamState.Free()
	encoder.vorbis.block.Free()
	encoder.vorbis.dspState.Free()
	encoder.vorbis.comment.Free()
	encoder.vorbis.info.Free()
}

// EncodeNinjamInterval will accept deinterleaved samples.
// Returns an array of arrays of bytes, one array per each packet generated.
func (encoder *Encoder) EncodeNinjamInterval(samples [][]float32) ([][]byte, error) {
	// validate len
	if len(samples) == 0 || len(samples) != encoder.ChannelCount {
		return nil, fmt.Errorf("Invalid length of samples[][]")
	}

	bufLen := len(samples[0])
	for i := 0; i < len(samples); i++ {
		if bufLen != len(samples[i]) {
			return nil, fmt.Errorf("Lengths of samples[] mismatch")
		}
	}

	// lengths are valid, proceed
	extra := (bufLen % encoder.ChunkSize) > 0
	nPackets := bufLen / encoder.ChunkSize
	if extra {
		nPackets += 1
	}

	res := make([][]byte, nPackets)

	first := true
	for p := 0; p < nPackets; p++ {
		var ninjamPacket []byte

		log.Debugf("Creating Ogg packet %v", p)

		// if this is our first packet, initialize vorbis headers
		if first {
			log.Debug("First packet")
			ninjamPacket, err := encoder.initVorbisHeaders()
			if err != nil {
				log.Debug("Could not init vorbis headers")
				return nil, fmt.Errorf("Coult not init vorbis headers")
			}
			log.Debugf("Appended %v bytes to packet", len(ninjamPacket))
			first = false
		}

		// deinterleave and analyze samples
		start := p * encoder.ChunkSize
		end := intmin(bufLen, (p + 1) * encoder.ChunkSize)
		buf := make([][]float32, encoder.ChannelCount)
		for c := 0; c < encoder.ChannelCount; c++ {
			buf[c] = samples[c][start:end]
		}
		log.Debugf("Analysing %v frames [%v:%v]", end - start, start, end)
		encoder.analyzeSamples(buf)

		log.Debug("Analysis complete, encoding stream")

		// encode our samples
		endOfStream := false
		for {
			if vorbis.AnalysisBlockout(&encoder.vorbis.dspState, &encoder.vorbis.block) != 1 {
				log.Debug("AnalysisBlockout returned value other than 1")
				break
			}
			vorbis.Analysis(&encoder.vorbis.block, nil)
			vorbis.BitrateAddblock(&encoder.vorbis.block)

			var packet vorbis.OggPacket
			defer packet.Free()

			for {
				if vorbis.BitrateFlushpacket(&encoder.vorbis.dspState, &packet) == 0 {
					log.Debug("Bitrate flush returned zero")
					break
				}
				vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &packet)
				for endOfStream == false {
					var page vorbis.OggPage
					defer page.Free()

					if vorbis.OggStreamFlush(&encoder.vorbis.streamState, &page) == 0 {
						log.Debug("OggStreamFlush returns 0")
						break
					}

					// ensure that page data is in sync with its C counterpart
					page.Deref()
					// modify slice length to be equal to one we expect
					resizeByteSlice(&page.Header, page.HeaderLen)
					resizeByteSlice(&page.Body, page.BodyLen)

					ninjamPacket = append(ninjamPacket, page.Header...)
					ninjamPacket = append(ninjamPacket, page.Body...)

					log.Debugf("Appended %v bytes to packet", len(page.Header) + len(page.Body))
					log.Tracef("Page contents: %v", page)

					if vorbis.OggPageEos(&page) != 0 {
						log.Debug("Reached end of Ogg stream")
						endOfStream = true
					}
				}
			}
		}
		log.Debugf("Final packet length: %v bytes", len(ninjamPacket))

		// store our new packet
		res[p] = ninjamPacket
	}

	// finalize and close the encoder
	encoder.clearVorbisHeaders()

	log.Debugf("Encoding complete")
	log.Debugf("Number of packets produced: %v", len(res))
	for i := 0; i < len(res); i++ {
		log.Debugf("Length of packet %v: %v bytes", i, len(res[i]))
	}

	// return result
	return res, nil
}
