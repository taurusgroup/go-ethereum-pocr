package cliquepocr

import (
	"errors"
	// "math"
	"math/big"

	"github.com/ethereum/go-ethereum/log"
	// "sort"
	// "github.com/ethereum/go-ethereum/log"
)

// The standard WhitePaper computation
type RaceRankComputation struct {
	rankArray []*big.Rat
}

func NewRaceRankComputation() IRewardComputation {
	return &RaceRankComputation{
		rankArray: []*big.Rat{big.NewRat(1, 1)},
	}
}

func (wp *RaceRankComputation) getRanking(rank int) *big.Rat {
	// calculate the rational value for the given index if it does not already exists
	previous := wp.rankArray[len(wp.rankArray)-1]
	for i := len(wp.rankArray); i <= rank; i++ {
		// multiply the previous one by 0,9
		previous = new(big.Rat).Mul(previous, big.NewRat(9, 10))
		wp.rankArray = append(wp.rankArray, previous)
	}
	return wp.rankArray[rank]
}

func (wp *RaceRankComputation) GetAlgorithmId() int {
	return 3
}

func (wp *RaceRankComputation) CalculateRanking(footprint *big.Int, nodesFootprint []*big.Int) (rank *big.Rat, nbNodes int, err error) {
	if footprint.Cmp(zero) <= 0 {
		return nil, 0, errors.New("cannot proceed with zero or negative footprint")
	}
	var NbItemsAbove int
	nbNodes = len(nodesFootprint)

	if nbNodes == 0 {
		return nil, 0, errors.New("cannot rank zero node")
	}
	// Sorting is not necessary if we parse the total list of the nodes
	for i := 0; i < nbNodes; i++ {
		// node footprint is lower than current node but not zero
		if nodesFootprint[i].Cmp(footprint) == -1 && nodesFootprint[i].Cmp(zero) > 0 {
			NbItemsAbove++
		}
	}

	rank = wp.getRanking(NbItemsAbove)

	log.Debug("Ranking calculation", "node footprint", footprint, "all footprints", nodesFootprint, "nb nodes above", NbItemsAbove, "rank", rank)

	return rank, nbNodes, nil
}

// On an annual basis, what is the minimimum amount of CRC tokens that have to be generated each year
// Inflation control speed at 10^6
var inflationDenominator = big.NewInt(10000000)

// Minimum creation of CRC per bloc 10^5 per year
var minCreationPerBlock = new(big.Rat).Mul(big.NewRat(100000, 365*24*3600/4), new(big.Rat).SetInt(CTCUnit))

func (wp *RaceRankComputation) CalculateGlobalInflationControlFactor(M *big.Int) (*big.Rat, error) {
	// L = TotalCRC / InflationDenominator
	// D = pow(alpha, L)
	// GlobalInflation  = 1/D

	// If the amount of crypto is negative raise an error
	if M.Sign() == -1 {
		return big.NewRat(0,1), errors.New("negative total crypto amount is not possible")
	}

	// If there is no crpto created, return 1
	if M.Cmp(zero) == 0 {
		return big.NewRat(1, 1), nil
	}

	L := new(big.Rat).SetFrac(M, new(big.Int).Mul(CTCUnit, inflationDenominator))

	L = L.Mul(L, big.NewRat(72, 100)) // mul by 0,72 to be able to apply the limited devt on alpha = 2
	// resolve the alpha^L in big.Int by using limited development formula
	// 𝛴 (x^k)/k! with 4 levels only
	D := big.NewRat(1, 1) // D = 1
	D = D.Add(D, L)       // 1 + L

	L2 := new(big.Rat).Mul(L, L)                          // L^2
	D = D.Add(D, new(big.Rat).Mul(L2, big.NewRat(1, 2)))  // + L^2 / 2
	L2 = L2.Mul(L2, L)                                    // L^3
	D = D.Add(D, new(big.Rat).Mul(L2, big.NewRat(1, 6)))  // + L^3 / 6
	L2 = L2.Mul(L2, L)                                    // L^4
	D = D.Add(D, new(big.Rat).Mul(L2, big.NewRat(1, 24))) // + L^3 / 24

	return D.Inv(D), nil
}

func (wp *RaceRankComputation) CalculateCarbonFootprintReward(rank *big.Rat, nbNodes int, totalCryptoAmount *big.Int) (*big.Int, error) {
	// In CRC Unit : 0.9^rank
	rewardCRCUnit := new(big.Rat).Mul(rank, new(big.Rat).SetInt(CTCUnit))

	// 0.9^rank x N
	rewardCRCUnit = rewardCRCUnit.Mul(rewardCRCUnit, big.NewRat(int64(nbNodes), 1))

	// 0.9^rank x N * Inflation
	inflationFactor, err := wp.CalculateGlobalInflationControlFactor(totalCryptoAmount)
	if err != nil {
		return nil, err
	}
	rewardCRCUnit = rewardCRCUnit.Mul(rewardCRCUnit, inflationFactor)

	// apply the minimum reward if needed
	if rewardCRCUnit.Cmp(minCreationPerBlock) == -1 {
		rewardCRCUnit = minCreationPerBlock
	}

	u := new(big.Int).Div(rewardCRCUnit.Num(), rewardCRCUnit.Denom())

	return u, nil
}
