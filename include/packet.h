/*
 * Copyright 2017 Nippon Telegraph and Telephone Corporation.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#ifndef LAGOPUS_PACKET_H_
#define LAGOPUS_PACKET_H_

#include <stdbool.h>
#include <rte_mbuf.h>
#include <rte_hash.h>
#include <rte_jhash.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef uint16_t vifindex_t;
typedef uint16_t vrfindex_t;

#define VIF_INVALID_INDEX	0
#define VIF_MAX_INDEX		4095	   // Valid VIF index is from 1 to VIF_MAX_INDEX
#define VIF_BROADCAST		0xfffffffc // OFPP_ALL

#define VRF_MAX_ENTRY		256	   // A maximum number of VRF entries

#define BRIDGE_INVALID_ID	0
#define BRIDGE_MAX_ID		1023	   // Valid Bridge ID is from 1 to BRIDGE_MAX_ID

#define MAX_PACKET_SZ 4096
#define PACKET_METADATA_SIZE (RTE_MBUF_PRIV_ALIGN << 6)	// Shall be a multiple of RTE_MBUF_PRIV_ALIGN

#define LAGOPUS_MBUF_METADATA(mbuf) (struct lagopus_packet_metadata*)((void*)(mbuf) + sizeof(struct rte_mbuf))

typedef enum {
	// The packet is sent to the router itself.
	LAGOPUS_MD_SELF			= (1 << 0),

	// The packet should be processed by MAT. Used by bridge module only.
	LAGOPUS_MD_MAT			= (1 << 1),
} lagopus_md_flag_t;

struct lagopus_packet_metadata {
	struct vif_metadata {
		uint64_t vrf;			// DEPRECATED. WILL BE REMOVED.
		vifindex_t in_vif;
		vifindex_t out_vif;

		lagopus_md_flag_t flags;	// Or'd LAGOPUS_MD_*

		bool local;			// True if the packet is originated locally. False otherwise.
	} md_vif;
	uint8_t udata[PACKET_METADATA_SIZE - sizeof(struct vif_metadata)];
};

#ifdef __cplusplus
}
#endif

#endif // LAGOPUS_PACKET_H_
