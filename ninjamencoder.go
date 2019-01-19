package ninjamencoder

import (
	"unsafe"

	"github.com/burillo-se/vorbis-go/vorbis"
)

// stuff internal to encoder that we need to pass around
type vorbisParam struct {
	Info        vorbis.Info
	StreamState vorbis.OggStreamState
	Comment     vorbis.Comment
	DSPState    vorbis.DspState
	Block       vorbis.Block
}

// EncoderParam parameters to use for vorbis encoder.
type EncoderParam struct {
	ChannelCount int     // number of audio channels to encode
	SampleRate   int     // audio sample rate
	ChunkSize    int     // number of frames in a single packet
	Quality      float32 // vorbis encoder quality
}

// NewEncoderParam creates a new EncoderParam struct with sensible default values:
// stereo 44.1kHz, chunk size set to 8K frames, quality set to 0.1
func NewEncoderParam() *EncoderParam {
	return &EncoderParam{
		2,     // default to stereo
		44100, // default to 44.1kHz sample rate
		8192,  // default to splitting interval into packets 8K frames each
		0.1,   // default to quality of 0.1
	}
}

// generics ftw
func intmin(a int, b int) int {
	if a > b {
		return a
	}
	return b
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

func analyzeSamples(encoderParam *EncoderParam,
	vorbisParam *vorbisParam, samples []float32) {
	// vorbis analysis buffer is managed by vorbis library
	nFrames := len(samples) / encoderParam.ChannelCount

	res := vorbis.AnalysisBuffer(&vorbisParam.DSPState, int32(nFrames))

	for i := 0; i < nFrames; i++ {
		for c := 0; c < encoderParam.ChannelCount; c++ {
			sampleIndex := i*encoderParam.ChannelCount + c

			// get a ptr to sample value, and write it
			samplePtr := getSamplePtr(res, c, i)
			*samplePtr = samples[sampleIndex]
		}
	}

	// notify vorbis that we've wrote the analysis buffer
	vorbis.AnalysisWrote(&vorbisParam.DSPState, int32(nFrames))
}

func initVorbisHeaders(encoderParam *EncoderParam, vorbisParam *vorbisParam) []byte {
	var headers []byte
	vorbis.InfoInit(&vorbisParam.Info)
	vorbis.EncodeInitVbr(&vorbisParam.Info,
		encoderParam.ChannelCount,
		encoderParam.SampleRate,
		encoderParam.Quality)

	vorbis.CommentInit(&vorbisParam.Comment)
	vorbis.CommentAddTag(&vorbisParam.Comment, "Encoder", "guitar-jam.ru")

	vorbis.AnalysisInit(&vorbisParam.DSPState, &vorbisParam.Info)
	vorbis.BlockInit(&vorbisParam.DSPState, &vorbisParam.Block)

	vorbis.OggStreamInit(&vorbisParam.StreamState, 0)

	var header vorbis.OggPacket
	headerComm := make([]vorbis.OggPacket, 1)
	headerCode := make([]vorbis.OggPacket, 1)

	vorbis.AnalysisHeaderout(&vorbisParam.DSPState, &vorbisParam.Comment,
		&header, headerComm, headerCode)

	vorbis.OggStreamPacketin(&vorbisParam.StreamState, &header)
	vorbis.OggStreamPacketin(&vorbisParam.StreamState, &headerComm[0])
	vorbis.OggStreamPacketin(&vorbisParam.StreamState, &headerCode[0])

	for {
		var page vorbis.OggPage
		result := vorbis.OggStreamFlush(&vorbisParam.StreamState, &page)
		if result == 0 {
			break
		}
		headers = append(headers, page.Header...)
		headers = append(headers, page.Body...)
	}

	return headers
}

// EncodeNinjamInterval will accept encoder parameters,
// and an array of (interleaved) samples. Returns an array
// of arrays of bytes, one array per each packet generated.
func EncodeNinjamInterval(encoderParam *EncoderParam,
	samples []float32) [][]byte {
	var vorbisParam vorbisParam
	first := true
	nPackets := len(samples) / encoderParam.ChannelCount / encoderParam.ChunkSize
	res := make([][]byte, nPackets)

	for p := 0; p < nPackets; p++ {
		var ninjamPacket []byte

		// if this is our first packet, initialize vorbis headers
		if first {
			ninjamPacket = initVorbisHeaders(encoderParam, &vorbisParam)
			first = false
		}

		// deinterleave and analyze samples
		samplesPerChunk := encoderParam.ChannelCount * encoderParam.ChunkSize
		start := p * samplesPerChunk
		end := intmin(len(samples), (p+1)*samplesPerChunk)
		analyzeSamples(encoderParam, &vorbisParam, samples[start:end])

		// encode our samples
		endOfStream := false
		for vorbis.AnalysisBlockout(&vorbisParam.DSPState, &vorbisParam.Block) != 0 {
			vorbis.Analysis(&vorbisParam.Block, nil)
			vorbis.BitrateAddblock(&vorbisParam.Block)

			var packet vorbis.OggPacket

			for vorbis.BitrateFlushpacket(&vorbisParam.DSPState, &packet) != 0 {
				vorbis.OggStreamPacketin(&vorbisParam.StreamState, &packet)
				for endOfStream == false {
					var page vorbis.OggPage

					if vorbis.OggStreamFlush(&vorbisParam.StreamState, &page) == 0 {
						break
					}

					ninjamPacket = append(ninjamPacket, page.Header...)
					ninjamPacket = append(ninjamPacket, page.Body...)

					if vorbis.OggPageEos(&page) != 0 {
						endOfStream = true
					}
				}
			}
		}

		// store our new packet
		res[p] = ninjamPacket
	}

	// finalize and close the encoder
	vorbis.OggStreamClear(&vorbisParam.StreamState)
	vorbis.BlockClear(&vorbisParam.Block)
	vorbis.DspClear(&vorbisParam.DSPState)
	vorbis.CommentClear(&vorbisParam.Comment)
	vorbis.InfoClear(&vorbisParam.Info)

	// return result
	return res
}
