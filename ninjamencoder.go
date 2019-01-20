package ninjamencoder

import (
	"unsafe"

	"github.com/xlab/vorbis-go/vorbis"
)

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

func (encoder *Encoder) analyzeSamples(samples []float32) {
	// vorbis analysis buffer is managed by vorbis library
	nFrames := len(samples) / encoder.ChannelCount

	res := vorbis.AnalysisBuffer(&encoder.vorbis.dspState, int32(nFrames))

	for i := 0; i < nFrames; i++ {
		for c := 0; c < encoder.ChannelCount; c++ {
			sampleIndex := i*encoder.ChannelCount + c

			// get a ptr to sample value, and write it
			samplePtr := getSamplePtr(res, c, i)
			*samplePtr = samples[sampleIndex]
		}
	}

	// notify vorbis that we've wrote the analysis buffer
	vorbis.AnalysisWrote(&encoder.vorbis.dspState, int32(nFrames))
}

func (encoder *Encoder) initVorbisHeaders() []byte {
	var headers []byte
	vorbis.InfoInit(&encoder.vorbis.info)
	vorbis.EncodeInitVbr(&encoder.vorbis.info,
		encoder.ChannelCount,
		encoder.SampleRate,
		encoder.Quality)

	vorbis.CommentInit(&encoder.vorbis.comment)
	vorbis.CommentAddTag(&encoder.vorbis.comment, "Encoder", "guitar-jam.ru")

	vorbis.AnalysisInit(&encoder.vorbis.dspState, &encoder.vorbis.info)
	vorbis.BlockInit(&encoder.vorbis.dspState, &encoder.vorbis.block)

	vorbis.OggStreamInit(&encoder.vorbis.streamState, 0)

	var header vorbis.OggPacket
	headerComm := make([]vorbis.OggPacket, 1)
	headerCode := make([]vorbis.OggPacket, 1)

	vorbis.AnalysisHeaderout(&encoder.vorbis.dspState, &encoder.vorbis.comment,
		&header, headerComm, headerCode)

	vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &header)
	vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &headerComm[0])
	vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &headerCode[0])

	for {
		var page vorbis.OggPage
		result := vorbis.OggStreamFlush(&encoder.vorbis.streamState, &page)
		if result == 0 {
			break
		}
		headers = append(headers, page.Header...)
		headers = append(headers, page.Body...)
	}

	return headers
}

func (encoder *Encoder) clearVorbisHeaders() {
	vorbis.OggStreamClear(&encoder.vorbis.streamState)
	vorbis.BlockClear(&encoder.vorbis.block)
	vorbis.DspClear(&encoder.vorbis.dspState)
	vorbis.CommentClear(&encoder.vorbis.comment)
	vorbis.InfoClear(&encoder.vorbis.info)
}

// EncodeNinjamInterval will accept an array of (interleaved) samples.
// Returns an array of arrays of bytes, one array per each packet generated.
func (encoder *Encoder) EncodeNinjamInterval(samples []float32) [][]byte {
	first := true
	nPackets := len(samples) / encoder.ChannelCount / encoder.ChunkSize
	res := make([][]byte, nPackets)

	for p := 0; p < nPackets; p++ {
		var ninjamPacket []byte

		// if this is our first packet, initialize vorbis headers
		if first {
			ninjamPacket = encoder.initVorbisHeaders()
			first = false
		}

		// deinterleave and analyze samples
		samplesPerChunk := encoder.ChannelCount * encoder.ChunkSize
		start := p * samplesPerChunk
		end := intmin(len(samples), (p+1)*samplesPerChunk)
		encoder.analyzeSamples(samples[start:end])

		// encode our samples
		endOfStream := false
		for vorbis.AnalysisBlockout(&encoder.vorbis.dspState, &encoder.vorbis.block) != 0 {
			vorbis.Analysis(&encoder.vorbis.block, nil)
			vorbis.BitrateAddblock(&encoder.vorbis.block)

			var packet vorbis.OggPacket

			for vorbis.BitrateFlushpacket(&encoder.vorbis.dspState, &packet) != 0 {
				vorbis.OggStreamPacketin(&encoder.vorbis.streamState, &packet)
				for endOfStream == false {
					var page vorbis.OggPage

					if vorbis.OggStreamFlush(&encoder.vorbis.streamState, &page) == 0 {
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
	encoder.clearVorbisHeaders()

	// return result
	return res
}
