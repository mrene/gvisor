// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin && arm64

package hvf

/*
#include <Hypervisor/Hypervisor.h>
#include <stdlib.h>
#include <string.h>

// saveFPRegs saves all 32 SIMD/FP Q registers plus FPCR/FPSR from
// the vCPU into the given buffer. The buffer layout matches Linux's
// fpsimd_context: fpsr(4) + fpcr(4) + vregs[32*16](512) = 520 bytes.
// The caller must pass a pointer to the fpsr field (offset 8 in
// the full fpsimd_context which has an 8-byte header).
static void saveFPRegs(hv_vcpu_t vcpu, void *buf) {
    uint32_t *ctrl = (uint32_t *)buf;
    uint8_t *vregs = (uint8_t *)buf + 8; // after fpsr + fpcr

    // FPSR and FPCR via the regular register API.
    uint64_t fpsr, fpcr;
    hv_vcpu_get_reg(vcpu, HV_REG_FPSR, &fpsr);
    hv_vcpu_get_reg(vcpu, HV_REG_FPCR, &fpcr);
    ctrl[0] = (uint32_t)fpsr;
    ctrl[1] = (uint32_t)fpcr;

    // Q0-Q31 (128-bit each).
    for (int i = 0; i < 32; i++) {
        hv_simd_fp_uchar16_t val;
        hv_vcpu_get_simd_fp_reg(vcpu, (hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), &val);
        memcpy(vregs + i * 16, &val, 16);
    }
}

// copyStatePageGPRegs copies X0-X15, X18-X30 from a state page into
// a uint64[31] buffer and reads X16, X17 from the vCPU via API.
static void copyStatePageGPRegs(hv_vcpu_t vcpu, const void *statePage, uint64_t *buf) {
    const uint64_t *sp = (const uint64_t *)statePage;
    for (int i = 0; i < 16; i++) buf[i] = sp[i];
    hv_vcpu_get_reg(vcpu, HV_REG_X16, &buf[16]);
    hv_vcpu_get_reg(vcpu, HV_REG_X17, &buf[17]);
    for (int i = 18; i < 31; i++) buf[i] = sp[i];
}

// saveGPRegs saves X0-X30 from the vCPU into a uint64[31] buffer.
// Single CGO call replaces 31 individual hv_vcpu_get_reg calls.
static void saveGPRegs(hv_vcpu_t vcpu, uint64_t *buf) {
    for (int i = 0; i < 31; i++) {
        hv_vcpu_get_reg(vcpu, (hv_reg_t)(HV_REG_X0 + i), &buf[i]);
    }
}

// loadGPRegs loads X0-X30 into the vCPU from a uint64[31] buffer.
static void loadGPRegs(hv_vcpu_t vcpu, const uint64_t *buf) {
    for (int i = 0; i < 31; i++) {
        hv_vcpu_set_reg(vcpu, (hv_reg_t)(HV_REG_X0 + i), buf[i]);
    }
}

// loadFPRegs loads all 32 SIMD/FP Q registers plus FPCR/FPSR into
// the vCPU from the given buffer (same layout as saveFPRegs).
static void loadFPRegs(hv_vcpu_t vcpu, const void *buf) {
    const uint32_t *ctrl = (const uint32_t *)buf;
    const uint8_t *vregs = (const uint8_t *)buf + 8;

    hv_vcpu_set_reg(vcpu, HV_REG_FPSR, (uint64_t)ctrl[0]);
    hv_vcpu_set_reg(vcpu, HV_REG_FPCR, (uint64_t)ctrl[1]);

    for (int i = 0; i < 32; i++) {
        hv_simd_fp_uchar16_t val;
        memcpy(&val, vregs + i * 16, 16);
        hv_vcpu_set_simd_fp_reg(vcpu, (hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), val);
    }
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/arch"
)

// Exit reason constants.
const (
	exitReasonException       = C.HV_EXIT_REASON_EXCEPTION
	exitReasonCanceled        = C.HV_EXIT_REASON_CANCELED
	exitReasonVtimerActivated = C.HV_EXIT_REASON_VTIMER_ACTIVATED
)

// maxUserAddress is the maximum user VA for HVF on ARM64.
// Accessed atomically to prevent data races with concurrent readers.
var maxUserAddress atomic.Uint64

func init() {
	maxUserAddress.Store((1 << 48) - 1)
}

// vectorsPageSize is the size of the exception vector page. Must be at least
// one page. On macOS ARM64, pages are 16K.
const vectorsPageSize = 16384

// initialize sets up the vCPU with exception vectors for EL0 guest execution.
// The approach:
//  1. Build an exception vector table at EL1 that forwards all exceptions
//     to the hypervisor via HVC.
//  2. Guest code runs at EL0. SVC traps to EL1, our handler does HVC to exit.
//  3. The hypervisor reads ESR_EL1 to determine the original exception type.
func (c *vCPU) initialize() error {
	// Enable floating point and SIMD access at EL0/EL1.
	if err := c.setSysReg(C.HV_SYS_REG_CPACR_EL1, 3<<20); err != nil {
		return fmt.Errorf("set CPACR_EL1: %w", err)
	}

	// Mask the virtual timer to prevent spurious timer interrupts.
	C.hv_vcpu_set_vtimer_mask(c.vcpuID, C.bool(true))
	// Keep the virtual timer disabled: el0_sync uses CNTV_CVAL_EL0 as a
	// scratch register for guest X16.
	if err := c.setSysReg(C.HV_SYS_REG_CNTV_CTL_EL0, 0); err != nil {
		return fmt.Errorf("set CNTV_CTL_EL0: %w", err)
	}
	// The guest's CNTVCT_EL0 is mach_absolute_time() minus this offset. The
	// VDSO computes time from CNTVCT_EL0 with parameters that the sentry
	// calibrates against mach_absolute_time() (see sentry/time.Rdtsc on
	// darwin), so the two must be the same counter.
	if ret := C.hv_vcpu_set_vtimer_offset(c.vcpuID, 0); ret != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpu_set_vtimer_offset: %d", ret)
	}

	// Point this vCPU to the shared exception vectors.
	if err := c.setSysReg(C.HV_SYS_REG_VBAR_EL1, c.machine.vectorsAddr); err != nil {
		return fmt.Errorf("set VBAR_EL1: %w", err)
	}

	// TTBR0_EL1 is set dynamically in Switch() for per-process page tables.
	// Initialize to 0 (no valid mappings until Switch sets it).
	if err := c.setSysReg(C.HV_SYS_REG_TTBR0_EL1, 0); err != nil {
		return fmt.Errorf("set TTBR0_EL1: %w", err)
	}

	// TTBR1_EL1: shared kernel page table for upper-half VAs (sentry memory).
	if err := c.setSysReg(C.HV_SYS_REG_TTBR1_EL1, c.machine.kernelPT.ttbr1()); err != nil {
		return fmt.Errorf("set TTBR1_EL1: %w", err)
	}

	// TCR_EL1: dual-TTBR split VA space, 48-bit VAs.
	// TG0: 4K (0x0) or 16K (0x2) depending on hvfPageSize.
	// TG1: always 16K (kernel page tables stay 16K).
	var tg0 uint64
	if hvfPageSize == 4096 {
		tg0 = 0x0 // 4K granule
	} else {
		tg0 = 0x2 // 16K granule
	}
	tcr := uint64(16) | // T0SZ=16 → 48-bit VA
		(0x1 << 8) | // IRGN0
		(0x1 << 10) | // ORGN0
		(0x3 << 12) | // SH0
		(tg0 << 14) | // TG0: 4K or 16K
		(uint64(16) << 16) | // T1SZ=16 → 48-bit VA
		(0x1 << 24) | // IRGN1
		(0x1 << 26) | // ORGN1
		(0x3 << 28) | // SH1
		(uint64(0x1) << 30) | // TG1: 16K (kernel stays 16K)
		(uint64(0x2) << 32) | // IPS: 40-bit PA
		(uint64(1) << 36) // AS: 16-bit ASID
	if err := c.setSysReg(C.HV_SYS_REG_TCR_EL1, tcr); err != nil {
		return fmt.Errorf("set TCR_EL1: %w", err)
	}

	// MAIR_EL1 and SCTLR_EL1 for per-process MMU.
	if err := c.setSysReg(C.HV_SYS_REG_MAIR_EL1, 0xFF); err != nil {
		return fmt.Errorf("set MAIR_EL1: %w", err)
	}
	// SCTLR_EL1: MMU enabled, caches, stack alignment.
	// UCI[26]=1: allow EL0 cache maintenance (DC CIVAC, DC CVAU, IC IVAU).
	// UCT[15]=1: allow EL0 read of CTR_EL0 (cache type register).
	// Without these, JVM's cache flush and feature detection trap to EL1.
	if err := c.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x34909185); err != nil {
		return fmt.Errorf("set SCTLR_EL1: %w", err)
	}

	// TPIDR_EL1: kernel VA of per-vCPU state page (in TTBR1).
	// Used by el0_sync handler to save X0-X30 and by ERET stub to load them.
	if err := c.setSysReg(C.HV_SYS_REG_TPIDR_EL1, c.statePageVA); err != nil {
		return fmt.Errorf("set TPIDR_EL1: %w", err)
	}

	// SP_EL1: set to end of state page. Not actively used by the
	// 0x200 handler (which does TLBI+ERET without memory access),
	// but must be valid for EL1h exception entry.
	if err := c.setSysReg(C.HV_SYS_REG_SP_EL1, c.statePageVA+16384-16); err != nil {
		return fmt.Errorf("set SP_EL1: %w", err)
	}

	// Write dispatch code page VA to state page for BLR from EL1 handler.
	binary.LittleEndian.PutUint64(
		(*[16384]byte)(c.statePageHost)[spOffsetDispatchVA:], c.machine.dispatchKVA)

	// CNTKCTL_EL1: enable EL0 access to the virtual counter (CNTVCT_EL0).
	// Bit 1 (EL0VCTEN): allow EL0 to read the virtual counter.
	// This is required for the VDSO clock_gettime implementation which
	// reads CNTVCT_EL0 directly from userspace.
	if err := c.setSysReg(C.HV_SYS_REG_CNTKCTL_EL1, 0x2); err != nil {
		return fmt.Errorf("set CNTKCTL_EL1: %w", err)
	}

	return nil
}

// setupSharedMemory allocates and maps vectors + page tables into the VM.
// These are shared across all vCPUs. Called once from newMachine().
func (m *machine) setupSharedMemory() error {
	// --- Exception vector table ---
	var vecMem unsafe.Pointer
	if ret := C.posix_memalign(&vecMem, C.size_t(vectorsPageSize), C.size_t(vectorsPageSize)); ret != 0 || vecMem == nil {
		return fmt.Errorf("failed to allocate vector page")
	}
	C.memset(vecMem, 0, C.size_t(vectorsPageSize))

	// ARM64 exception vector table layout (VBAR_EL1-relative offsets).
	// Each of the 16 entries is 128 bytes; entry i exits to the host with
	// HVC #(vectorHVCBase+i) unless replaced below:
	//
	//   0x000-0x180: Current EL, SP_EL0 (sync, IRQ, FIQ, SError)
	//   0x200-0x380: Current EL, SP_ELx; 0x200 (sync) is the TLB retry
	//                handler below
	//   0x400-0x580: Lower EL, AArch64; 0x400 (sync) is el0_sync, which
	//                exits with HVC #9 for SVC and HVC #8 for other
	//                exceptions
	//   0x600-0x780: Lower EL, AArch32 (unused); 0x600 holds the rest of
	//                the el0_sync SVC path
	//
	// Starting the generic immediates at vectorHVCBase keeps them distinct
	// from the el0_sync exits: an IRQ taken from EL0 (0x480) must not look
	// like a syscall.
	vectors := make([]byte, vectorsPageSize)
	for i := range 16 {
		hvcInstr := uint32(0xd4000002) | (uint32(vectorHVCBase+i) << 5)
		binary.LittleEndian.PutUint32(vectors[i*128:], hvcInstr)
	}
	// 0x200 (current-EL SPx sync): TLB fault recovery.
	// When STP to TTBR1 state page faults (cold D-TLB after TLBI),
	// flush TLB and retry. No SPSR/ELR save needed here because
	// the el0_sync slow path saves guest ELR→X17 and SPSR→X18
	// before starting the STP chain. This handler preserves all
	// GP registers (no memory access), so X17/X18 survive. The
	// current-EL exception automatically sets ELR=faulting STP
	// and SPSR=EL1 PSTATE, which is exactly what ERET needs.
	{
		off := 0x200
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		put(0xd508831f) // TLBI VMALLE1IS
		put(0xd5033b9f) // DSB ISH
		put(0xd5033fdf) // ISB
		put(0xd69f03e0) // ERET
	}

	// el0_sync at 0x400: read ESR_EL1, classify exception.
	// SVC (EC=0x15): save X0-X30 to state page, exit HVC #9.
	// Other: save ESR to X18, exit HVC #8.
	// State page mapped in TTBR1 with AP[1]=0 (EL1-only), so PAN
	// does not block access. TPIDR_EL1 holds state page kernel VA.
	//
	// Linux preserves every EL0 GP register except the syscall result
	// across exceptions, and compilers use X16-X18 as ordinary
	// temporaries. The handler therefore first stashes guest X16-X18 in
	// EL1-owned system registers (see msrCNTVCVALX16 et al.), without touching
	// memory, and every return path restores them.
	{
		off := 0x400
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		put(msrCNTVCVALX16) // MSR CNTV_CVAL_EL0, X16
		put(movSPX17)       // MOV SP, X17 (SP_EL1)
		put(msrTPIDRROX18)  // MSR TPIDRRO_EL0, X18
		// SVC continues at 0x600, which saves the guest registers and
		// exits. No syscall is handled inside the VM: every return to EL0
		// goes through an entry stub (and its TLBI), and every EL1
		// handler path ends in an HVC exit. Switch relies on both.
		put(0xd5385212) // MRS X18, ESR_EL1
		put(0xd35afe51) // LSR X17, X18, #26
		put(0x7100563f) // CMP W17, #0x15     (SVC?)
		put(0x54000041) // B.NE .+8           (→ fault)
		put(encodeB(off, 0x600))
		// fault: save ESR to X18, exit via HVC #8.
		// No STP chain for faults — TLB always cold after ASIDE1IS,
		// recovery overhead (~300ns) outweighs API savings.
		put(0xd5385212) // MRS X18, ESR_EL1
		put(0xd4000102) // HVC #8
		if off > 0x480 {
			panic(fmt.Sprintf("el0_sync handler overflows its vector slot: %#x", off))
		}
	}

	// SVC save path at 0x600 (AArch32 vector space, unused).
	{
		off := 0x600
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		// Save guest ELR/SPSR to X17/X18 before the STP chain.
		// If the STP chain triggers a TLB fault (0x200 handler),
		// SPSR/ELR system regs are overwritten by the current-EL
		// exception. But the simplified 0x200 handler (TLBI+ERET)
		// preserves all GP registers, so X17/X18 survive. The STP
		// chain then stores X18 to the state page. The sentry reads
		// X17 from API (guest PC), X18 from state page (guest
		// PSTATE), and guest X16-X18 from the vector scratch
		// registers.
		put(0xd5384031) // MRS X17, ELR_EL1
		put(0xd5384012) // MRS X18, SPSR_EL1
		// Save X0-X30 to state page, then HVC #9.
		// Full STP chain. Cold TLB → fault to 0x200 → TLBI+ERET retry.
		put(0xd538d090) // MRS X16, TPIDR_EL1
		stpEnc := func(rt1, rt2, rn, byteOff int) uint32 {
			return 0xA9000000 | uint32(((byteOff/8)&0x7F)<<15) | uint32(rt2<<10) | uint32(rn<<5) | uint32(rt1)
		}
		put(stpEnc(0, 1, 16, 0x00))
		put(stpEnc(2, 3, 16, 0x10))
		put(stpEnc(4, 5, 16, 0x20))
		put(stpEnc(6, 7, 16, 0x30))
		put(stpEnc(8, 9, 16, 0x40))
		put(stpEnc(10, 11, 16, 0x50))
		put(stpEnc(12, 13, 16, 0x60))
		put(stpEnc(14, 15, 16, 0x70))
		put(stpEnc(18, 19, 16, 0x90))
		put(stpEnc(20, 21, 16, 0xA0))
		put(stpEnc(22, 23, 16, 0xB0))
		put(stpEnc(24, 25, 16, 0xC0))
		put(stpEnc(26, 27, 16, 0xD0))
		put(stpEnc(28, 29, 16, 0xE0))
		put(0xF9000000 | uint32((0xF0/8)<<10) | uint32(16<<5) | 30) // STR X30
		put(0xd4000122) // HVC #9
	}

	// Entry stubs. The first does TLBI ASIDE1IS by the current ASID, then
	// ERET. GP regs are loaded via HVF API (loadGPRegs) before entry. X17 is
	// needed for the ASID, so stash the guest value in SP_EL1.
	{
		off := entryStubOff
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		put(movSPX17)      // MOV SP, X17
		put(0xd5382011)    // MRS X17, TTBR0_EL1 (ASID)
		put(0xd5088351)    // TLBI ASIDE1IS, X17
		put(0xd5033b9f)    // DSB ISH
		put(0xd5033fdf)    // ISB
		put(movX17SP)      // MOV X17, SP
		put(msrTPIDRROXZR) // MSR TPIDRRO_EL0, XZR
		put(0xd69f03e0)    // ERET

		// Full TLBI stub — used on ASID wrap.
		m.fullTLBIStubOff = uint64(off)
		put(0xd508831f)    // TLBI VMALLE1IS
		put(0xd5033b9f)    // DSB ISH
		put(0xd5033fdf)    // ISB
		put(msrTPIDRROXZR) // MSR TPIDRRO_EL0, XZR
		put(0xd69f03e0)    // ERET
		m.entryStubsEnd = uint64(off)
	}

	C.memcpy(vecMem, unsafe.Pointer(&vectors[0]), C.size_t(len(vectors)))

	m.vectorsAddr = 0
	m.vectorsMem = vecMem
	ret := C.hv_vm_map(vecMem, C.hv_ipa_t(m.vectorsAddr), C.size_t(vectorsPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	if ret != C.HV_SUCCESS {
		C.free(vecMem)
		return fmt.Errorf("hv_vm_map vectors failed: %d", ret)
	}

	// Per-process page tables are allocated in NewAddressSpace()
	// via the ptPageAllocator. No global page table needed.

	return nil
}

// buildDispatchCode writes the EL1 dispatch handler to the dispatch
// code page. Called via BLR from the el0_sync vector for non-fast-path
// syscalls. Returns X0=result, X1=0(handled via ERET) or X1=1(exit).
// Has access to state page via MRS TPIDR_EL1 (already loaded in X16
// by the calling vector handler).
func (m *machine) buildDispatchCode() error {
	code := (*[16384]byte)(m.dispatchMem)

	// Dispatch code page: called via BLR from el0_sync vector handler.
	// X8 = syscall number, X0-X5 = args, X16 = state page VA.
	// Returns: X9=0 (handled, vector does ERET) or X9=1 (exit via HVC #9).
	//
	// Currently: pure pass-through (all syscalls exit to host).
	// The dispatch page infrastructure is proven by el1gotest.
	// Future: add handlers for rt_sigprocmask, rt_sigaction, etc.
	binary.LittleEndian.PutUint32(code[0:], 0xd2800029) // MOV X9, #1
	binary.LittleEndian.PutUint32(code[4:], 0xD65F03C0) // RET

	return nil
}

// State page layout (16K per vCPU, pointed to by TPIDR_EL1).
// EL1 handler saves registers here; host reads via statePageHost pointer.
// All offsets must be 16-byte aligned for STP/LDP.
const (
	spOffsetGPRegs  = 0x000 // X0-X30: 31 * 8 = 248 bytes
	spOffsetESR     = 0x100 // ESR_EL1 saved by handler
	spOffsetSP_EL0  = 0x108 // SP_EL0 (MRS from EL1)
	spOffsetTPIDR   = 0x110 // TPIDR_EL0
	spOffsetPC         = 0x118 // ELR_EL1 (via API — returns 0 from EL1 MRS)
	spOffsetPSTATE     = 0x120 // SPSR_EL1 (via API)
	spOffsetSigMask    = 0x128 // Signal mask (uint64, synced by host)
	spOffsetSigDirty   = 0x130 // Non-zero if EL1 handler modified signal mask
	spOffsetDispatchVA = 0x200 // Kernel VA of dispatch code page
)

// The el0_sync vector stashes guest X16-X18 in EL1-owned system registers
// before using them as temporaries. None of these accesses touch memory, so
// they cannot fault before the exception syndrome has been captured.
//   - X16: CNTV_CVAL_EL0. The virtual timer is disabled and EL0 cannot
//     access it (CNTKCTL_EL1.EL0VTEN=0).
//   - X17: SP_EL1. EL1 code in the vectors page never uses a stack.
//   - X18: TPIDRRO_EL0. It is cleared before every return to EL0, as Linux
//     does for native AArch64 tasks.
const (
	msrCNTVCVALX16 = 0xd51be350 // MSR CNTV_CVAL_EL0, X16
	movSPX17       = 0x9100023f // MOV SP, X17
	movX17SP       = 0x910003f1 // MOV X17, SP
	msrTPIDRROX18  = 0xd51bd072 // MSR TPIDRRO_EL0, X18
	msrTPIDRROXZR  = 0xd51bd07f // MSR TPIDRRO_EL0, XZR
)

const (
	// vectorHVCBase is the HVC immediate of exception vector 0; vector i
	// exits with HVC #(vectorHVCBase+i) unless it has its own handler.
	// Exit HVCs must not use immediate 0: HVF answers HVC #0 itself,
	// without exiting, when W0 holds certain SMCCC function IDs (e.g.
	// 0xC1xxxxxx), and live guest X0 is often such a value.
	vectorHVCBase = 0x10

	// entryStubOff is the vectors-page offset of the first entry stub.
	// The stubs end at machine.entryStubsEnd.
	entryStubOff = 0x810
)

// encodeB returns the ARM64 encoding for an unconditional branch from the
// instruction at vectors-page offset from to offset to.
func encodeB(from, to int) uint32 {
	return 0x14000000 | uint32((to-from)/4)&0x03ffffff
}

// loadRegisters loads application registers from arch.Context64 into the vCPU
// and sets up the EL1-to-EL0 transition via ERET. FP/SIMD registers are only
// loaded if loadFP is set: otherwise the vCPU still holds the application's
// FP state from its last exit, which the sentry has not changed.
func (c *vCPU) loadRegisters(ac *arch.Context64, loadFP bool) {
	regs := &ac.Regs

	C.loadGPRegs(c.vcpuID, (*C.uint64_t)(unsafe.Pointer(&regs.Regs[0])))
	c.setSysReg(C.HV_SYS_REG_SP_EL0, regs.Sp)
	c.setSysReg(C.HV_SYS_REG_TPIDR_EL0, regs.TPIDR_EL0)

	if loadFP {
		fpData := ac.FloatingPointData()
		if fpData != nil && len(*fpData) >= 528 {
			C.loadFPRegs(c.vcpuID, unsafe.Pointer(&(*fpData)[8]))
			runtime.KeepAlive(fpData)
		}
	}

	c.setSysReg(C.HV_SYS_REG_ELR_EL1, regs.Pc)
	spsr := regs.Pstate &^ 0xf
	c.setSysReg(C.HV_SYS_REG_SPSR_EL1, spsr)

	eretStub := c.machine.vectorsAddr + entryStubOff
	if c.asidWrapped {
		eretStub = c.machine.vectorsAddr + c.machine.fullTLBIStubOff
		c.asidWrapped = false
	}
	c.setReg(C.HV_REG_PC, eretStub)
	c.setReg(C.HV_REG_CPSR, 0x3c5)
}

// saveRegisters saves vCPU registers back to arch.Context64.
// When gpInStatePage is set (HVC #9 exits with STP chain), GP regs
// are read from the state page. Otherwise falls back to API calls
// (for EC=0x18 traps, direct data aborts, etc. that bypass our handler).
// When gpInVectorScratch is set (HVC #8/#9 from el0_sync), guest X16-X18
// are read from the vector scratch registers.
func (c *vCPU) saveRegisters(ac *arch.Context64) {
	regs := &ac.Regs

	usedSTPChain := c.gpInStatePage
	if usedSTPChain {
		C.copyStatePageGPRegs(c.vcpuID, c.statePageHost,
			(*C.uint64_t)(unsafe.Pointer(&regs.Regs[0])))
		c.gpInStatePage = false
	} else {
		C.saveGPRegs(c.vcpuID, (*C.uint64_t)(unsafe.Pointer(&regs.Regs[0])))
	}

	regs.Sp = c.getSysReg(C.HV_SYS_REG_SP_EL0)
	if usedSTPChain {
		// STP chain path: the el0_sync handler saved guest ELR→X17
		// and SPSR→X18 before the STP chain. The 0x200 TLB fault
		// handler (TLBI+ERET) preserves all GP registers, so X17/X18
		// survive even if the STP chain faulted and retried.
		regs.Pc = regs.Regs[17]
		regs.Pstate = regs.Regs[18] &^ 0xf
	} else {
		regs.Pc = c.getSysReg(C.HV_SYS_REG_ELR_EL1)
		regs.Pstate = c.getSysReg(C.HV_SYS_REG_SPSR_EL1) &^ 0xf
	}
	if c.gpInVectorScratch {
		regs.Regs[16] = c.getSysReg(C.HV_SYS_REG_CNTV_CVAL_EL0)
		regs.Regs[17] = c.getSysReg(C.HV_SYS_REG_SP_EL1)
		regs.Regs[18] = c.getSysReg(C.HV_SYS_REG_TPIDRRO_EL0)
		c.gpInVectorScratch = false
	}
	regs.TPIDR_EL0 = c.getSysReg(C.HV_SYS_REG_TPIDR_EL0)

	// Skip FP save on syscall exits (HVC #9) — FP stays in vCPU,
	// re-loaded on next entry. Only save on fault exits (HVC #8)
	// which may trigger signal delivery that needs FP context.
	if c.saveFP {
		fpData := ac.FloatingPointData()
		if fpData != nil && len(*fpData) >= 528 {
			C.saveFPRegs(c.vcpuID, unsafe.Pointer(&(*fpData)[8]))
			runtime.KeepAlive(fpData)
		}
	}
}

// saveEL0Registers saves the application state of a vCPU that exited to the
// host directly from EL0: an asynchronous hv_vcpus_exit, or an exception
// routed to the hypervisor instead of the EL1 vectors. The state is in the
// registers themselves; ELR_EL1/SPSR_EL1 still describe the last entry, and
// the el0_sync scratch registers are not in use.
func (c *vCPU) saveEL0Registers(ac *arch.Context64) {
	c.gpInStatePage = false
	c.gpInVectorScratch = false
	c.saveRegisters(ac)
	ac.Regs.Pc = c.getReg(C.HV_REG_PC)
	ac.Regs.Pstate = c.getReg(C.HV_REG_CPSR) &^ 0xf
}

// atEL0 returns true if the vCPU stopped at EL0 (AArch64 EL0t).
func (c *vCPU) atEL0() bool {
	return c.getReg(C.HV_REG_CPSR)&0x1f == 0
}

// inEntryStub returns true if pc (at EL1) is in an entry stub, i.e. the vCPU
// is between loadRegisters and its return to EL0.
func (m *machine) inEntryStub(pc uint64) bool {
	return pc >= m.vectorsAddr+entryStubOff && pc < m.vectorsAddr+m.entryStubsEnd
}

// getExitReason returns the exit reason from the vCPU exit info.
func (c *vCPU) getExitReason() C.uint32_t {
	return C.uint32_t(c.exit.reason)
}

// getExceptionSyndrome returns the exception syndrome (ESR) from the
// vCPU exit information. This is valid when the exit reason is
// HV_EXIT_REASON_EXCEPTION.
func (c *vCPU) getExceptionSyndrome() uint64 {
	return uint64(c.exit.exception.syndrome)
}

// getFaultAddress returns the faulting virtual address from the vCPU
// exit information. This is valid for data/instruction abort exceptions.
func (c *vCPU) getFaultAddress() uint64 {
	return uint64(c.exit.exception.virtual_address)
}

// setReg sets a general-purpose or special register on the vCPU.
// Errors are not checked — these calls are on the hot path (31 regs
// per Switch iteration) and HVF register operations don't fail in
// practice for valid vCPU handles and register IDs.
func (c *vCPU) setReg(reg C.hv_reg_t, val uint64) {
	C.hv_vcpu_set_reg(c.vcpuID, reg, C.uint64_t(val))
}

// getReg gets a general-purpose or special register from the vCPU.
func (c *vCPU) getReg(reg C.hv_reg_t) uint64 {
	var val C.uint64_t
	C.hv_vcpu_get_reg(c.vcpuID, reg, &val)
	return uint64(val)
}

// setSysReg sets a system register on the vCPU.
func (c *vCPU) setSysReg(reg C.hv_sys_reg_t, val uint64) error {
	ret := C.hv_vcpu_set_sys_reg(c.vcpuID, reg, C.uint64_t(val))
	if ret != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpu_set_sys_reg(%d) failed: %d", reg, ret)
	}
	return nil
}

// getSysReg gets a system register from the vCPU.
func (c *vCPU) getSysReg(reg C.hv_sys_reg_t) uint64 {
	var val C.uint64_t
	C.hv_vcpu_get_sys_reg(c.vcpuID, reg, &val)
	return uint64(val)
}
